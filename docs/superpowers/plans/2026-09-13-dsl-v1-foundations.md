# DSL v1 foundations Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land epic 1 of the DSL version-one program (memql#5356, tasks #5357-#5362) as one PR: every tree declares its language line, editions select a front end, the annotation registry is the one gate for every construct, the attribute matrix is generated and pinned, the conformance corpus runs with two completeness gates, and the VS Code extension cannot silently lag the cluster's grammar.

**Architecture:** `component/language` owns the language: `parser` gains the language line, the edition table and the one annotation check wired into every construct and field parser; `annotations` becomes the registry of every receiver with argument forms and examples; `dslspec` derives from both. The loader in `component/memql` reads each domain's `memql.toml` and parses each file through its edition's front end. `test/conformance/2026` is data a Go runner holds the engine to. The extension pins the grammar it was built from, and the cluster's `ServerHello` says which grammar it speaks.

**Tech Stack:** Go 1.26 multi-module workspace; TypeScript (VS Code extension, `sdk/ts`); protobuf (`component/grpc/memql.proto`).

**Spec:** `docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md` (section 4, epic 1; D4, D6, D16, D23, D25). Read it before any task.

## Global Constraints

- One PR, branch `epic/dsl-v1-foundations`, against `main`; closes #5356 #5357 #5358 #5359 #5360 #5361 #5362. This plan is deleted in the PR's last commit.
- Language line `memql = "1.0"`; edition `"2026"` (`parser.LanguageVersion`, `parser.Edition`).
- The language package owns the language: no annotation table outside `component/language` when this lands.
- Every refusal names the construct or position, the annotation or rule, and what to write instead (D24). Refusals from the shared check carry a stable code in square brackets at the end: `[annotation_unknown]`.
- Verify with `make test` or `go test github.com/znasllc-io/memql/<path>/...`, never `go test ./...` from the root. Run `go test -count=1 .` (root gates) and `go test -count=1 ./scripts/ci/...` after adding any file, AFTER `git add` (several gates walk `git ls-files`).
- Stage files by explicit path; never `git add -A`. No emojis anywhere.
- New Go imports across modules must hold under `GOWORK=off` (`scripts/ci/module-boundaries.sh`); after `app/` or wiring changes build all seven node tags.
- `gofmt -w` only the files you touched.
- Product bundle repositories (memql-znas, memql-fylo, memql-project, memql-casera) are out of scope; they adopt the line in their own epics after this merges.
- Do not touch the running k3d cluster `memql` (another session owns it).

## File structure

| Path | Responsibility | Task |
|---|---|---|
| `component/language/parser/edition.go` | LanguageVersion, Edition, the front-end table | 0 (done) |
| `component/language/tiers/tiers.go` | the eleven expression positions | 0 (done) |
| `core/dslfs/manifest.go` | reads `memql.toml` (two-key TOML subset) | 0 (done) |
| `cmd/memqlmigrate/rewrites.go` | the rewrite registry keyed by edition and epic; `language-line` | 0 (done) |
| `component/memql/conformance_probe.go` | the corpus's seam onto today's lowering and post-filter; `LoadReport()` | 0 (done) |
| `test/conformance/corpus_test.go`, `engine_adapter_test.go`, `fuzz_test.go`, `corpus_manifest_test.go`, `2026/` | the corpus and its runner | 0 (done), 5 |
| `dsl/memql.toml`, `dsl/embed.go` | the embedded tree's line | 1 |
| `component/memql/language_line.go` | resolve every domain's line; refusals; the per-file hook | 1 |
| `examples/*/dsl/memql.toml` | each example pack declares its line | 1 |
| `component/memql` loaders, `component/automations` loader, `component/actions` loaders, `component/memql/dslimports` | parse each file through its edition's front end | 2 |
| `component/language/annotations/registry.go` (+ new files) | every receiver, forms, keys, examples, retirements; `Check` | 3 |
| `component/language/parser/*decl*.go`, `args_block_parser.go`, `parser.go` (action, capability, prompt and builtin fields, function attributes) | call the shared check | 3 |
| `component/database/memory-nodes/concept_parser.go` | concept and concept-field gate reads the registry | 3 |
| `component/language/dslspec/*` | derive constructs, body clauses and field lists from the parser tables | 3 |
| `component/memql/annotation_registry_consistency_test.go` | every receiver against its real gate | 3 |
| `component/language/dslspec/matrix.go`, `cmd/docs-gen` or a new cmd, `docs/public/language/attribute-matrix.md`, `attribute_matrix_parity_test.go`, `Makefile` | the generated matrix and its drift gate | 4 |
| `test/conformance/2026/cells/**`, `expr/**`, gates | the seeded cells and the two completeness gates | 5 |
| `editors/vscode/package.json`, `CHANGELOG.md`, `src/version/editionSkew.ts`, `src/connection/manager.ts`, `src/extension.ts`, tests | the pins and the connect-time notice | 6 |
| `cmd/memql-lsp/internal/grammar/*`, `cmd/memql-lsp/editorparity_test.go`, `Makefile` | language-configuration generator, staleness and parity gates | 6 |
| `component/grpc/memql.proto`, `server.go`, `sdk/ts/src/client/{wire,connection}.ts`, `sdk/go/client/connection.go` | the handshake carries edition, grammar version, editor release | 6 |

---

### Task 0: The seams (DONE, commits 519560618 87cbde282 665b1062f fd035899b)

Shared with the dsl-v1-expressions session (memql-10), who branches off this tip.

- [x] `parser.LanguageVersion = "1.0"`, `parser.Edition = "2026"`, `FrontEnd{Edition, GrammarVersion, Prepare}`, `FrontEndFor`, `Editions`, `RegisterEdition(fe) (unregister func())`.
- [x] `tiers.Position` and the eleven positions (`queryFilter sort specBody rowAuthzArgument automationCondition triggerFilter logicBody mutationValue stepArgument toolDefault promptInput`), `Positions()`.
- [x] `dslfs.ManifestFile = "memql.toml"`, `dslfs.Manifest{Language, Edition}`, `ParseManifest`, `Manifest.Render`, `*dslfs.ManifestError{Line, Msg}`.
- [x] `cmd/memqlmigrate`: `registry []rewrite{name, edition, epic, doc; plain | path | tree}`, `--edition` (default `parser.Edition`), usage errors exit 2 naming the registered set; `language-line` tree rewrite.
- [x] Corpus runner: five verdicts, `expect.json`, `fixture.memql`, one overlay domain per case, batched loads, `ExprEnv` adapter, `FuzzParse`, `TestCorpusManifestMatchesTheEngine`; `test/conformance/**` in the `go` CI bucket.

---

### Task 1: The language line in the loader (#5357)

**Files:**
- Create: `dsl/memql.toml`, `component/memql/language_line.go`, `component/memql/language_line_test.go`, `examples/{deploypack,referencepack,reviewspack,shopifypack}/dsl/memql.toml`
- Modify: `dsl/embed.go` (embed `memql.toml`; export `EmbeddedManifest() (dslfs.Manifest, error)` and `IsCoreDomain(string) bool`), `dsl/runtime_mount.go` and `dsl/lint_mount.go` (warn / report a root-level `memql.toml`, which no mount reads), `component/memql/engine_bootstrap.go` (resolve lines first; problems become report skips), `component/memql/unified_loader.go` (`BuildUnifiedConcepts` refuses a problem domain's concepts), `component/memql/lint_parity.go` (dedupe identical diagnostics), `embed_inventory_test.go` (counts +1 each), `.github/workflows/ci.yml` (`examples/shopifypack/dsl/**` in the bucket beside the other packs), every test that registers or mounts a throwaway tree (21 files under `component/memql`, `component/automations`, `test/dslconformance`; plus `component/packages`, `cmd/memqllint`, `deploy/fleet` fixtures as they fail), `docs/public/language/memql.md` (new section "The language line" after "Two Authoring Surfaces", ```toml fences), `docs/public/language/authoring-rules.md` (the versioning section: language line, editions, the migrator; fix its three stale claims)

**Interfaces:**
- Consumes: `dslfs.ParseManifest`, `dslfs.Manifest.Render`, `parser.LanguageVersion`, `parser.Edition`, `parser.FrontEndFor`.
- Produces:
  ```go
  // component/memql/language_line.go
  type LanguageLine struct {
      Domain   string // first path segment of the tree
      Source   string // "dsl/memql.toml" for a core domain, "<domain>/memql.toml" otherwise
      Language string
      Edition  string
      Embedded bool
  }
  type LanguageLineProblem struct {
      Domain, Source string
      Code    string // language_line_missing | language_line_malformed | language_version_newer | language_version_unsupported | edition_unknown
      Message string // ends with " [<Code>]"
  }
  // ResolveLanguageLines reads every domain's declaration. It never fails outright:
  // a problem domain is also returned in lines, with the engine's own edition, so
  // parsing proceeds and one boot reports every problem.
  func ResolveLanguageLines(tree fs.FS) (map[string]LanguageLine, []LanguageLineProblem)
  // LanguageLineFor returns the declaration governing a file path of the loaded tree:
  // the hook later epics key version-dependent meaning on.
  func (e *MemQLEngine) LanguageLineFor(path string) (LanguageLine, bool)
  ```

**Rules:**
- A core domain (`dsl.IsCoreDomain`) speaks the embedded manifest. Every other domain must carry `<domain>/memql.toml`; there is no inheritance from a bundle root.
- Both keys are required. `memql` newer than `parser.LanguageVersion` (compare major.minor numerically) refuses naming both. A version that is not `<digits>.<digits>` is malformed. An older version is `language_version_unsupported` (this engine reads only 1.0), naming the supported line. An edition `parser.FrontEndFor` refuses is `edition_unknown`, naming the editions.
- Messages (exact prefixes, the corpus and tests pin them):
  - missing: `domain "znas" declares no language line: add znas/memql.toml containing` + the two rendered lines + `(or run: memqlmigrate --rewrite=language-line <tree>) [language_line_missing]`
  - newer: `domain "znas" declares memql = "1.1", newer than the 1.0 this engine speaks: run an engine that speaks 1.1, or declare memql = "1.0" in znas/memql.toml [language_version_newer]`
  - edition: `domain "znas" declares edition = "2027", which this engine does not read (it reads: 2026) [edition_unknown]`
- Problems are `baseloader.Skip{Component: "languageLine", Keyword: "domain", Name: <domain>, File: <Source>, Phase: "language-line", Err: <Message>}`, added before the loaders run, so strict boot refuses and `MEMQL_DSL_ALLOW_SKIPS` is the break-glass. `BuildUnifiedConcepts` skips a problem domain's concepts with the same message, so the boot refuses before any concept reaches the database.

- [ ] **Step 1:** Write `language_line_test.go` first: (a) the embedded tree resolves every core domain to `parser.LanguageVersion` / `parser.Edition` from `dsl/memql.toml`; (b) a registered overlay without `memql.toml` makes `Init` refuse with the missing-line message naming the file to add; (c) `memql = "1.1"` refuses naming 1.1 and 1.0; (d) `edition = "2027"` refuses naming 2026; (e) `MEMQL_DSL_ALLOW_SKIPS=1` boots with the problem reported; (f) a bundle mounted at `MEMQL_DSL_PATH` (temp dir) without the line refuses boot through `MountRuntimeDomainsFromEnv` + `BuildUnifiedConcepts` + `Init`; (g) `LanguageLineFor("<domain>/queries.memql")` returns the declaration. Run: `go test -count=1 -run LanguageLine github.com/znasllc-io/memql/component/memql` -- FAIL.
- [ ] **Step 2:** Implement `dsl/memql.toml` (with a two-line comment), the embed, `EmbeddedManifest`, `IsCoreDomain`, `language_line.go`, and the two hooks. Run the tests -- PASS.
- [ ] **Step 3:** Add `memql.toml` to the four example packs and to every throwaway tree the suite registers; run `make test` and fix each failure by adding the line (never by weakening the check). Update `embed_inventory_test.go` counts.
- [ ] **Step 4:** Docs: memql.md section (what the line is, where it lives and why per domain, the refusals, the migrator command); authoring-rules.md versioning section. Run `go test -count=1 .` (docs gates, snippets).
- [ ] **Step 5:** Commit: `Issue #5357: every tree declares its language line; the loader refuses one missing or newer`.

### Task 2: Editions at every parse site (#5358)

**Files:**
- Modify: `component/memql/baseloader/loader.go` (ReadAll applies a per-path prepare hook), `component/memql/unified_loader.go` (concepts), `component/automations/unified_loader.go`, `component/actions/loader.go` and `catalog.go`, `component/memql/capability_loader.go`, `component/memql/dslimports/dslimports.go` (offline parse, memqllint)
- Create: `component/memql/edition_frontend_test.go`

**Interfaces:**
- Consumes: `ResolveLanguageLines` (Task 1), `parser.FrontEndFor`, `parser.RegisterEdition`.
- Produces: `func (l LanguageLines) Prepare(path string, src []byte) ([]byte, error)` (or an equivalent single function every loader calls), applied to every tree file before the struct-form rewriter, keyed by the file's root domain.

- [ ] **Step 1:** Write `TestTwoEditionsLoadInOneEngine`: register a synthetic edition `2099` whose `Prepare` rewrites a keyword only it knows (`predicate NAME {` to `trait NAME {`); mount domain `alpha` declaring 2026 (a trait) and domain `omega` declaring 2099 (a `predicate`); `Init` loads both, both traits register; the same `predicate` source in `alpha` is refused. Unregister in `t.Cleanup`. Run -- FAIL.
- [ ] **Step 2:** Thread the prepare step through every loader listed. Run -- PASS. Run `make test`.
- [ ] **Step 3:** Commit: `Issue #5358: each file is parsed through its domain's edition`.

### Task 3: One annotation registry (#5359)

**Files:**
- Modify: `component/language/annotations/registry.go` (+ `receivers.go`, `forms.go`, `check.go`, `retired.go` as the split warrants), `core/baseparser/iface.go` (its retired / misplaced tables move into `annotations`; delete `ValidateConstructAnnotations` if nothing else needs it), `component/language/parser/{decl_annotations,tool_decl,provider_decl,seed_decl,args_block_parser,parser,rule_decl,policy_decl,spec_decl}.go`, `component/database/memory-nodes/concept_parser.go`, `component/memql/{function_annotation_allow_lists,unified_functions_loader,builtin_converter,prompt_converter,shape_converter,spec_converter}.go`, `component/automations/loader.go`, `component/memql/sense/*` (consumers), `component/language/dslspec/*`, `dsl/platform/mutations.memql` (delete the orphaned `@actor("system")`)
- Modify/Create tests: `component/memql/annotation_registry_consistency_test.go` (every receiver), `component/language/annotations/*_test.go`, `component/language/dslspec/drift_test.go`, parser tests whose pinned messages move, `component/language/parser/grammar_surface_drift_test.go` (new narrowing entries), `grammar_version.go` (bump)

**Model (in `annotations`, still a leaf module):**
```go
type Receiver string // "Query" "Mutation" "Logic" "Automation" "Action" "Capability" "Spec" "Tool" "Builtin" "Prompt" "Provider" "Shape" "Policy" "Rule" "Seed" "Concept" "ConceptBody" "ConceptField" "ArgsField" "ToolField" "PromptField" "BuiltinField"
type Form uint16 // bit set: FormFlag FormEmpty FormString FormStrings FormNumber FormKeywords FormObject FormExpression FormExclude
type Placement struct {
    Receiver   Receiver
    Name       string
    Forms      Form
    Keys       []ArgSpec // closed key set for FormKeywords; a bare flag key has Type "flag"
    Repeatable bool
    Example    string    // canonical use on this receiver, e.g. `@cache(300)`
    Doc        string    // receiver-specific doc; empty falls back to Docs[Name]
}
type Use struct { Name string; Form Form; Keys []string } // one written annotation, as the parser saw it
type Refusal struct { Code, Message string }             // Error() = Message + " [" + Code + "]"
const (CodeUnknown = "annotation_unknown"; CodeRetired = "annotation_retired"; CodeMisplaced = "annotation_misplaced"; CodeForm = "annotation_form"; CodeKey = "annotation_key"; CodeRepeated = "annotation_repeated")
func Check(r Receiver, u Use) *Refusal
func CheckAll(r Receiver, uses []Use) *Refusal      // adds the repeat rule
func Placements() []Placement                        // every placement, receiver order then name
func Lookup(r Receiver, name string) (Placement, bool)
func Receivers() []Receiver
func (r Receiver) Phrase() string                   // "a query", "an args field", ...
var ByReceiver map[string][]string                   // DERIVED from the placements; key "Concept" replaces ""
```
- Refusal order for a name not accepted on `r`: retired (for `r` or every receiver) with its hint; else accepted on other receivers -> misplaced naming them; else unknown with a did-you-mean from `r`'s names and the full list.
- Form messages are generated from `Forms` and `Example`: `@cache on a query takes one number, as in @cache(300)`.
- The placements must accept every form the in-tree corpus writes (measured census: `cmd`-free script in the session scratchpad; re-run it over `dsl/`, `examples/`, and the two local product bundles before fixing a form). Known outliers: `@when()` (FormEmpty on rule), `@filter(<expr>)` (FormExpression | FormString), `@rowAuthz(owner="x", clusterOwner)` (keywords with flag keys), `@cache(300)` and `@cache(ttl="300")`, `@visibility` (retired: read by nothing).

**Wiring:** a parser helper `parser.annotationUse(*ast.Attribute) annotations.Use` feeds the check from every construct parser at parse time (shape, builtin, prompt, policy, rule, spec, trait, tool, provider, seed, action, capability, and function attributes in `attachAttributes`); field lists call it per field (`ArgsField`, `ToolField`, `PromptField`, `BuiltinField`); `concept_parser.go` calls it for `Concept`, `ConceptBody` (`@relationship`) and `ConceptField`. The per-construct switches keep argument SEMANTICS only; their default branches go. The load-time text gate and its allow-list plumbing are deleted once the parse-time check covers the four function kinds.

- [ ] **Step 1:** Write the registry tests first: every placement's `Example` parses to a `Use` the check accepts; a derived wrong-form variant is refused with `annotation_form` or `annotation_key`; an unknown name is refused with a did-you-mean; every retired name refuses with its hint; `ByReceiver` equals the placements. FAIL.
- [ ] **Step 2:** Implement the model and `Check`. PASS.
- [ ] **Step 3:** Rewrite `annotation_registry_consistency_test.go` to drive EVERY receiver's real gate: for each placement, a minimal construct of the receiver carrying the example is accepted by the parser (and, for concept receivers, by the concept translator); a wrong-form variant and an unknown name are refused with the right code. FAIL until wired.
- [ ] **Step 4:** Wire every parser and the concept translator; delete the duplicate tables and the load-time text gate; fix the orphaned `@actor("system")`. Run `make test`, `go run ./cmd/memqllint dsl/`, and `TestStrictBoot_EmbeddedTreeIsClean`.
- [ ] **Step 5:** `dslspec`: `BodyBlocks` from exported parser clause tables (query: args filter shape sort paginate asOf count; mutate: args insert update accept stamp; logic: args body; automation: args step precondition; action / capability: args; provider: params auth); construct list from `parser.StructFormKeywords` + `parser.TopLevelDeclKeywords` + `use`; `FieldAnnotations` from the field receivers; clause keywords in the lexicon from the same tables; fix the stale next-rule and snippet entries the survey found (`filter {` inserted as a block, `body {` offered inside an automation, `rule` missing from top level). Extend `drift_test.go` to assert BodyBlocks equal the clause tables for every construct.
- [ ] **Step 6:** Add grammar-surface corpus entries for each narrowing (unknown annotation on a prompt field, unknown annotation on a builtin field, a flag given an argument) and bump `GrammarVersion` to `2026.09-dsl-v1-foundations-<digest>`; regenerate the TextMate grammar (`make vscode-grammar`).
- [ ] **Step 7:** Commit: `Issue #5359: one annotation registry gates every construct and field`.

### Task 4: The generated attribute matrix (#5360)

**Files:**
- Create: `component/language/dslspec/matrix.go` (+ test), a `docs-matrix` subcommand (extend `cmd/docs-gen` if it fits, else `cmd/attributematrix`)
- Modify: `docs/public/language/attribute-matrix.md` (generated), `attribute_matrix_parity_test.go` (byte comparison), `Makefile` (`docs-matrix`, `docs-matrix-check`, each with a `## ` doc line and `.PHONY`), the registry docs that `declared_metadata_annotations_test.go` requires to appear (`@secret` prose moves into the `ConceptField` `secret` placement doc), `docs/public/language/{functions,specifications,reserved}.md` (stale hand tables point at the matrix)

**Page design (the frontend-design pass):** front matter; a two-sentence lead saying the page is generated from the registry and the command that regenerates it; "At a glance" as four compact matrices by family (Functions: query mutate logic automation; Data: concept shape spec seed; AI: prompt provider policy rule tool; Capabilities: builtin action capability) plus one for fields (concept field, args field, tool field, prompt field, builtin field), rows grouped by what the annotation does, cells `yes` or empty; then "By construct" listing each receiver's annotations with the canonical example; then "Annotations", one entry per name (`### @cache`): where accepted, argument form in words, the keyword table when it has keys, the example per receiver, the doc; then "Retired" (name, where, what to write instead). No emojis, no decorative labels; every heading is a real anchor.

- [ ] **Step 1:** Test first: `TestAttributeMatrixIsGenerated` compares the committed file byte for byte with `dslspec.AttributeMatrix()`; a second assertion that every placement appears and no name the check refuses appears. FAIL.
- [ ] **Step 2:** Implement the renderer and the command, then run the new docs-matrix target. PASS. Run `go test -count=1 .` and the makefile help gate.
- [ ] **Step 3:** Commit: `Issue #5360: the attribute matrix is generated from the registry and pinned`.

### Task 5: The corpus cells and the two completeness gates (#5361)

**Files:**
- Create: `test/conformance/corpus_gates_test.go`, `test/conformance/2026/cells/<receiver>/<annotation>/{fixture.memql, <positive>.memql, <negative>.memql, expect.json}` for every placement, `test/conformance/2026/expr/<position>/` for the ten positions not yet seeded, a cell scaffolder (`go test ./test/conformance -run TestCorpusCells -scaffold` writes missing cells from the placements and the check's own messages; it never overwrites an existing file)

**Gates:**
- `TestCorpusCoversEveryRegistryCell`: for every placement, `cells/<receiver lowerCamel>/<annotation>/` has at least one case with verdict `load_ok` and at least one `refuse_parse` or `refuse_load`; the failure lists every missing cell; a cell directory no placement names also fails.
- `TestCorpusCoversEveryTierPosition`: for every `tiers.Position`, `expr/<position>/` has at least one positive and one negative case.

- [ ] **Step 1:** Write the two gates. FAIL, naming every missing cell and position.
- [ ] **Step 2:** Scaffold the cells, then read every generated case and correct any whose skeleton is unidiomatic; seed each position with one form that loads today and one that is refused today (measure each refusal message by running the runner, then pin it). PASS.
- [ ] **Step 3:** Negative controls: delete one cell's negative case, confirm the gate names it; restore.
- [ ] **Step 4:** Commit: `Issue #5361: every registry cell and every expression position has a case that loads and one that is refused`.

### Task 6: Editor parity (#5362)

**Files:**
- Modify: `editors/vscode/package.json` (top-level `"memql": {"edition": "2026", "grammarVersion": "<parser.GrammarVersion>"}`; version bump to the release that carries this grammar), `editors/vscode/CHANGELOG.md` (a section for that version naming the grammar version and edition, and the new notice), `component/language/parser/edition.go` (`const EditorRelease`: the first extension release that carries GrammarVersion), `component/grpc/memql.proto` (`ServerHello`: `edition = 5`, `grammar_version = 6`, `editor_release = 7`), `component/grpc/server.go` (stamp them), generated `component/grpc/gen/memql.pb.go` (`make proto-gen`), `sdk/ts/src/client/{wire,connection}.ts`, `sdk/go/client/connection.go` (+ fakes), `editors/vscode/src/connection/manager.ts` (getters), `editors/vscode/src/extension.ts` (the notice on "connected", once per cluster and grammar per session), `editors/vscode/test/support/vscodeStub.ts` (record warning actions), `cmd/memql-lsp/internal/grammar/` (generate `language-configuration.json` from dslspec's lexicon plus structured rules), `Makefile` (`vscode-grammar` writes both files), `docs/public/language/vscode.md`
- Create: `editors/vscode/src/version/editionSkew.ts` (no `vscode` import) + `test/editionSkew.test.ts`, `cmd/memql-lsp/editorparity_test.go`, `cmd/memql-lsp/internal/grammar/languageconfig.go` + staleness test

**Any other client (D25):** `dslspec.Spec` gains `Edition` and `GrammarVersion` (from the parser), so the `DslSpec` export over the stream answers the same question for a client that is not the extension; `SpecVersion` moves to `1.1.0`.

**Parity gate (`cmd/memql-lsp/editorparity_test.go`):** fails when `memql.grammarVersion` differs from `parser.GrammarVersion`, when `memql.edition` differs from `parser.Edition`, when `parser.EditorRelease` is newer than `package.json` `version`, or when `CHANGELOG.md` has no `## <EditorRelease>` section naming `GrammarVersion` and `edition <Edition>`. Each message names the exact edit.

**Notice (pure module `editionSkew.ts`):**
```ts
export type LanguageSkewState = "unknown" | "match" | "clusterNewer" | "clusterOlder" | "differs";
export interface LanguageFacts { edition?: string; grammarVersion?: string; editorRelease?: string; version?: string }
export interface LanguageSkew { state: LanguageSkewState; headline: string; details: string[]; releaseToInstall?: string }
export function compareLanguage(cluster: LanguageFacts, extension: LanguageFacts): LanguageSkew
```
- unknown (the cluster reported no edition): no notice. match: no notice.
- clusterNewer (cluster edition later, or same edition, different grammar and `editorRelease` newer than this extension's version): warning `This cluster's MemQL grammar is newer than this extension's. Update MemQL for VS Code to <editorRelease> or newer so completion and diagnostics match the cluster.` (edition case: `This cluster speaks MemQL edition <e>; this extension speaks edition <e'>. Update MemQL for VS Code to <editorRelease> or newer.`) Actions: `Open in Extensions` (runs `extension.open` for `znasllc.memql`), `Show details`.
- clusterOlder: information `This cluster's MemQL grammar is older than this extension's. The editor may suggest forms this cluster refuses.` Action: `Show details`.
- differs (no order can be shown): information naming both grammar versions.
- Details (output channel): cluster and extension edition and grammar, the release that carries the cluster's grammar.

- [ ] **Step 1:** Tests first: `editionSkew.test.ts` for every state and that each headline names both sides or the release; `manager.test.ts` getters from `fakeConn`; the Go parity test; the language-configuration staleness test. FAIL.
- [ ] **Step 2:** Implement: proto + regenerate + stamp; SDK copies; getters; the pure module; the wiring in `extension.ts`; the generator. PASS. Run `make vscode-test`, `go test -count=1 ./cmd/memql-lsp/...`, `make proto-gen-check` (after commit), `npm run typecheck` in `sdk/ts`.
- [ ] **Step 3:** Commit: `Issue #5362: the extension pins the grammar it was built from and compares it with the cluster at connect`.

### Task 7: Integration and ship

- [ ] Merge the task branches in order 1, 2, 3, 6, 4, 5; resolve conflicts by reading both sides.
- [ ] Final `GrammarVersion` (digest) once; `make vscode-grammar`; `parser.EditorRelease`, the extension version, `memql.grammarVersion` and the CHANGELOG section agree; regenerate the matrix (docs-matrix target); `make arch-model`; `make proto-gen-check`.
- [ ] Verification matrix, all green, output read: `make test`; `MEMQL_REQUIRE_DB=1` db-gated trees against a real Postgres when one is reachable; the seven node-tag builds; `scripts/ci/module-boundaries.sh`; `go run ./cmd/memqllint dsl/`; `make vscode-test`; `make sdk-ts-typecheck`; `gitleaks dir .` over the diff; `go test -count=1 . ./scripts/...`.
- [ ] Delete this plan; open the PR (body: `Closes #5356` ... one line each, the carried design record #5355, the wire change note for the frontend); watch CI; merge through the queue or `scripts/dev/merge-as-owner.sh`; close any issue the merge did not; remove the worktree and local branches.
