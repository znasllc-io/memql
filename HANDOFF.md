# HANDOFF — epic memql#5375, DSL v1 attribute cleanup

Written at the end of a long session. The next session has no memory of it.

---

## 1. GIT STATE

| | |
|---|---|
| Branch | `epic/dsl-v1-attributes` |
| HEAD | `a5ddeb07f` — "fix(campaigns): restore the XSS fixture's expected-escaped string" |
| Commits ahead of `origin/main` | 28 |
| Uncommitted changes | **none** (working tree clean) |
| Pushed | **NO.** The branch exists only locally. `git ls-remote --heads origin epic/dsl-v1-attributes` returns nothing. |
| PR | **none opened** |
| GitHub issues | **untouched** — #5375, #5376, #5377, #5378, #5379 are all still OPEN with no comments from this work |

A tag `archive/dsl-deploy-authoring` points at `d255af24f`, the one deleted
local branch that carried a commit not patch-identical to main. Delete it once
you are satisfied nothing is lost.

**Local cleanup already done** (the user asked for it explicitly): 8 stale
worktrees pruned, 11 stale branches deleted. `git branch` now shows only `main`
and `epic/dsl-v1-attributes`; `git worktree list` shows only the main checkout.
Nothing further to clean.

---

## 2. DONE / PARTIAL / BROKEN

### WORKING — the epic's substance is complete

- **The retirement ledger.** `core/baseparser/iface.go` holds
  `retiredConstructAnnotations` + `retiredFieldAnnotations`, and every hint ends
  with the `AttributeRewriteHint` constant so every refusal names
  `memqlmigrate --rewrite=attributes`.
- **Retirements landed** in the registry, the parsers and the loaders:
  `@deprecated`, `@timeout`, `@retry`, `@idempotent`, `@audit`, `@version` (on
  functions only), `@unique`, `@immutable`, `@rateLimit`, `@scopes`,
  `@latestMode`, `@enabled`, `@namespace`, `@default` (on concept fields only),
  `import (...)`, `;`-as-AND.
- **Single spellings**: `@cache(N)` with `@cache(0)`; `@trigger(schedule=)`;
  `mutation` (both positions); provider `@vendor`.
- **Dead `FunctionDef` fields deleted** along with their whole downstream chain
  — the runtime `Function` fields, the `help()` render in
  `executor_builtin.go`, and the editor hover in `sense/hover.go`.
- **Field allow-lists**: prompt and builtin fields now get the one `args` fields
  have (`component/memql/function_annotation_allow_lists.go`).
- **`cmd/memqlmigrate --rewrite=attributes`** (+ `attributes-namespace`), with
  20 test cases including comment-prose protection and idempotence.
- **The whole `dsl/` tree is migrated.**
  `go run ./cmd/memqllint dsl/` → **308 files, no diagnostics.** Re-run this
  first; it is the fastest proof the engine half is intact.
- **Matrix + gates**: `docs/public/language/attribute-matrix.md` has a
  `## Retired attributes` section, pinned in both directions by four new tests
  in `attribute_matrix_parity_test.go`.

### PARTIAL — the test-fixture tail

Green (verified individually, after all fixes):
`component/memql`, `component/language/parser`, `component/language/ast`,
`component/language/compiler`, `component/language/dslspec`,
`component/memql/sense`, `component/memql/dslimports`,
`component/memql/callgraph`, `component/automations`, `component/campaigns`,
`component/database/memory-nodes`, `core/baseparser`, `cmd/memqlmigrate`,
`cmd/shopifyschema`, `sdk/gen`, `dsl`, `test/dslconformance`.

**Still failing, not yet touched** (from the last targeted run):

| Package | Failing tests |
|---|---|
| `component/campaigns` | `TestConsentSuppressRequiresReason` ("recordConsentSuppress missing"), `TestDeliveryIdDerivationMatchesTheMutation` (looks for the literal `"mutate delivery recordCampaignDelivery"`) |
| `component/grpc` | `TestAuthoringValidate_OK`, `TestAuthoringSessionDefine_InjectsCallable` — origin-less `AuthorSessionBundle`/`ValidateBundle` calls, same class as the ones already fixed in `component/memql` |
| `integrations/library` | `dsl_fidelity_test.go` — already half-fixed (its comment says `mutation`); the derivation regex inside it probably still says `mutate` |
| `test/authoring` | `TestCrossRef_SelfConsistentBundlePasses` and siblings — origin-less authoring calls |
| `cmd/memql-lsp` | `TestDefinition_JumpsToDeclarationInAnotherFile` (line offset), `TestTrainingState_HandleProducesContractJSON` (JSON expects `"kind":"mutate"` and a signature-range width) |
| root `github.com/znasllc-io/memql` | last seen failing on `TestConceptFieldSnapshotIsNotStale`, **since fixed** by `make concept-snapshot`; re-verify |

### BROKEN / PRE-EXISTING — not mine, not fixed

`make test` is **red on pristine `main`** for three packages. Do not chase these
and do not count them as regressions:

- `scripts/ci` — `ruleset-drift.sh:151: KNOWN_ABSENT[@]: unbound variable` (bash 3.2 on macOS)
- `scripts/install` — host reports `darwin/amd64`; the installer targets `linux/amd64,darwin/arm64`
- `scripts/lib` — sudo-askpass GUI tests need a terminal

---

## 3. DECISIONS MADE — do not re-litigate

The design record's D17 says "retired because nothing reads them". **Four of
those claims were false**, found by running the re-verification section 10 of
the record demands. Each is kept, with its reader named in its registry doc
string, and `TestRetiredSetIsTheD17Set` in `core/baseparser` fails the build if
one is added back.

1. **`@allowedRoles` is KEPT.** D17 says replace it with `@requiresRank` +
   `@requiresCapability`. Those gate the **human actor's** catalog rank and their
   grants over a resource. `@allowedRoles` gates which **AGENT role**
   (`assistant` / `specialist`) may call a tool, and it is enforced on every path
   (`tool_types.go:196`, `grpc/server.go:2360`, `tool_execution.go:583`).
   Substituting rank for agent role would let every specialist call the
   assistant-only tools — `dsl/skills/seeds/foundational.memql:39` depends on the
   distinction in as many words. **Recorded as an open program decision for the
   owner; not executed.**
2. **`@displayCard` and `@composable` are KEPT.** This is issue #5378's answer.
   `@displayCard` → `clients/os/src/apps/concepts/displayCard.ts` +
   `RowsPanel.tsx`, and `test/dslconformance/displaycard_inventory_test.go`
   requires every concept to declare or decline one. `@composable` →
   `clients/os/src/apps/materializer/useCompose.ts` via
   `integration.compose.composableConcepts`.
3. **`@default` is retired on a CONCEPT field only.** On a `tool` / `prompt` /
   `builtin` field the body **is** the JSON schema handed to the model, so
   `default` there is a value the model reads. The codemod is construct-aware for
   exactly this reason.
4. **`,`-as-OR is DEFERRED to epic memql#5363, not retired.** It is live grammar:
   two filter expressions fold into ONE argument at a traversal call
   (`parentOf(concept==v1:rel:hub, concept==v1:rel:space)`), and
   `parseOrPipeOnly`'s own comment calls `(a, b)` a "still-supported OR form".
   Its codemod is `--rewrite=expressions`, which #5363 owns. Retiring it here
   would ship a refusal whose migration does not exist. `;`-as-AND **was**
   retired — it had no live producer left. The deferral is recorded in
   `parseLogicalOr`, in `retired_cascade_forms_test.go`, and in the matrix.

Two more decisions that changed behaviour deliberately:

5. **`namespace.pin` is now the DERIVATION, not a cross-check.**
   `ast.AssembleConceptIdFromDeclInDir` previously consulted the pin only to
   VALIDATE an explicit `@namespace`, and the absent case fell straight to the
   directory. That was sound while the annotation existed and became
   load-bearing the moment it did not: `dsl/shopify/generated` is nested (so its
   directory is not a legal namespace at all) and `dsl/deployment` pins
   `"cluster"`. A **present** `@namespace` is now the retirement refusal, which
   also had to win over the #2614 moved-file guard — that guard told an author to
   "fix the annotation" that no longer exists.
6. **`v1:authoring:construct` gains an `origin` field.** The schema literally
   documented "a promoted concept row re-derives its canonical id from the
   source's `@namespace` at re-hydration". Retiring the annotation broke that
   silently — every promoted concept failed to re-register on the next boot.
   `origin` (the tree-relative path the bundle was authored against) carries the
   namespace instead. Additive and optional, so no stored row is bricked; the
   concept-field snapshot gained exactly **1 line, 0 deletions**.

Consequence of #5 worth knowing: a **colon-scoped sub-namespace** was a
per-CONCEPT annotation and the pin is per-DIRECTORY, so a sub-namespace now
needs its own sub-directory. One fixture in `sdk/gen/concepts_test.go` relied on
the per-concept form.

---

## 4. KEY FILES

**The mechanism**

| Path | Why |
|---|---|
| `core/baseparser/iface.go` | The two retirement ledgers + `AttributeRewriteHint`. Every refusal in the epic routes through here. Add a retirement here first. |
| `component/language/annotations/registry.go` | `ByReceiver` / `Docs` / `KeywordArgs`. Dropping a name here is what turns it from silently-folded into refused for the four function constructs. |
| `cmd/memqlmigrate/attributes.go` | The codemod. **Construct-aware** — `ownerOfLine` resolves which construct body each line sits in, because `@default`, `@type` and `@version` survive on some receivers and not others. |

**Production code changed for behaviour, not cosmetics** — these are the ones a
reviewer should read closely:

| Path | Change |
|---|---|
| `component/language/ast/concept_id.go` | Pin becomes the derivation; present `@namespace` refuses as retired. |
| `component/language/parser/rewriter.go` | `mutationStructHeader` adopts `mutation`; `buildStructQueryExpr` glues with `&&` not `;`; a refusing stage for the retired `mutate`. |
| `component/language/parser/parser.go` | `processFunctionAttributes` loses six dead folds; `semicolonIsAConnective` distinguishes the retired connective from the still-tolerated terminator. |
| `component/language/parser/actor_binding.go` | Header regex keyed on `mutation` — it matched nothing, so `RewriteActorBinding` had silently stopped adding `@actor`. |
| `component/memql/dslgate/clause.go` | `ConstructHeaderRe` keyed on `mutation`. **This is the row-authz classifier's matcher** — keyed on the wrong word it sees no mutations and the gate passes everything. |
| `component/memql/function_slices.go` | The top-level slicer regex + the keyword→kind map. |
| `component/memql/function_loader.go` | `signatureConceptRe`; `LatestMode` derived from the body alone. |
| `component/language/compiler/automation_generator.go` | `expressionToString` emits `&&`/`||`, not `;`/`,`. It renders parsed expressions **back to source** and the result is re-parsed. |
| `component/memql/seed_materializer.go` | `buildPerUserDedupQuery` emits `&&`. |
| `sdk/gen/resolve.go` + `gen.go` | Namespace derivation in lockstep with the engine; `constructHeader` slices `mutation`. Was emitting no client method for any mutation. |
| `component/memql/authoring_{session,promote_durable,staged}.go` | `origin` threaded end to end. |
| `dsl/authoring/{concepts,mutations,shapes}.memql` | The `origin` field, its mutation arg, and its shape projection. |

**Docs / gates**

`docs/public/language/attribute-matrix.md` (retirements section),
`attribute_matrix_parity_test.go` (4 new gates),
`docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md`
(D17 gains the verification findings), `CLAUDE.md`,
`docs/public/language/authoring-rules.md`, `dsl/_reference/*.memql`.

**Two files deliberately rewritten small**

- `test/dslconformance/default_stamped_test.go` — was ~500 lines pairing every
  concept-field `@default` against the mutations writing its concept, because
  "the author never finds out". The load refusal now tells them. Cut to one
  corpus assertion.
- `component/database/memory-nodes/declared_metadata_annotations_test.go` — the
  memql#2960 tripwire fired in the direction it did not anticipate (retirement,
  not enforcement). Narrowed to `@secret`, its one enforced survivor.

---

## 5. WHAT WAS TESTED — and what was NOT

**Verified:**
- `go run ./cmd/memqllint dsl/` → 308 files, no diagnostics. Re-run this first.
- `go build ./...` and `go vet ./...` clean.
- The 17 packages listed green in §2, each run individually after its fixes.
- `make concept-snapshot` regenerated: +1 line, 0 deletions.

**NOT verified — do not assume these work:**
- **A full green `make test` has never been observed on this branch.** The last
  complete run was 184 ok / 41 failing; several packages were fixed after it and
  six were never re-run together. Assume the §2 "still failing" list is
  incomplete until you see one clean run.
- **Nothing db-gated ran.** `MEMQL_REQUIRE_DB=1` was never set and no Postgres
  was reachable, so every `*_db_test.go` self-skipped. `scripts/ci/db-gated-packages.sh --trees`
  names the set CI actually runs; none of it has been exercised.
- **No cluster run.** `make up` was never invoked. Nothing cross-node,
  nothing in `test/clustere2e`, no boot of a real engine against a real database.
- **`clients/os` was never built or run.** `@displayCard` / `@composable` were
  verified by grep only.
- **The VS Code extension** was not built; only its TextMate grammar was
  regenerated (`editors/vscode/syntaxes/memql.tmLanguage.json`).

---

## 6. GOTCHAS / RISKS

1. **WIRE CONTRACT CHANGE — needs a frontend ping.** `;` was accepted as AND in
   **raw client-supplied query strings** (`concept==X;payload.active==true`), not
   only in authored DSL. A client sending the old spelling now gets a refusal.
   `&&` has always been accepted in the same position, so the client fix is a
   find-and-replace. CLAUDE.md requires this called out in the PR body.
2. **Never regex-sweep `;` or `,` across `*.go`.** Two separate incidents:
   - A `;`→`&&` pattern with `\s*` before the identifier matched Go's own
     `if err := ...; err != nil` and corrupted **57 files**. Caught by the
     compiler, reverted — but the revert also discarded uncommitted fixture
     fixes. **Commit before any bulk edit.**
   - A narrower version still matched `;alert(1)` inside the HTML entity
     `&lt;script&gt;alert(1)&lt;/script&gt;` and rewrote an **XSS test's
     expected-escaped constant**. Fixed in HEAD; a repo-wide scan found no other
     entity damaged. If you sweep again, exclude HTML entities explicitly.
3. **Never `gofmt -w .` repo-wide.** It reformatted *checked-in generated* Go
   (1910 lines of `integrations/shopify/generated/model.go`), which then differed
   from its generator's output. Caught by `cmd/shopifyschema`'s drift test and
   fixed by regenerating. Format only the files you touched.
4. **`dsl/_reference/` is not embedded and never loads.** It is the deliberate
   don't-do-this skeleton tree. Its retired forms are labelled, not stripped.
5. **The `@enabled` sweep ate struct-literal elements.** `"@enabled",` was a
   slice element and a `strings.Replace` search argument in at least two places.
   Both fixed, but if a test fails with "too few values in struct literal" or a
   wrong argument count, that is the cause.
6. **Some fixtures depend on an annotation for an unstated reason.** A leading
   annotation is what puts the parser on the *declaration* route, where a typo'd
   keyword gets its did-you-mean; without one the source parses as a bare
   expression. Three fixtures needed `@description("d")` restored for this.
7. **`?.` still lexes and is still the arg-conditional filter marker** in a body
   position. This epic only improved the refusal's wording at the expression
   entry point. Do not delete `TokenQuestionDot`.
8. **Assumption that may be wrong:** the authoring fixtures were given origins
   like `"trainingns/concepts.memql"` chosen by matching the `v1:<ns>:` ids each
   file asserts. If a test asserts an id this heuristic guessed wrong, the origin
   is the thing to change.

---

## 7. DO NOT TOUCH

- **`@allowedRoles`, `@displayCard`, `@composable`** — kept on purpose (§3).
  `TestRetiredSetIsTheD17Set` fails the build if you "finish the job".
- **`,`-as-OR in `parseLogicalOr`** — deferred to #5363 on purpose. The comment
  in the function explains why; do not complete it here.
- **`@default` on tool / prompt / builtin fields** — those bodies are the schema.
- **`AttrEnabled`, `AttrNocache`, `AttrRateLimit`, `AttrSchedule` consts** — kept
  as parse-time *names* so the decl parsers can refuse them by name with a hint.
  They are not dead code.
- **The three `scripts/*` test failures** — pre-existing and environmental.
- **`archive/dsl-deploy-authoring`** — a safety tag, the user's call to delete.
- **`docs/superpowers/plans/2026-09-14-dsl-v1-attributes.md`** — the epic says
  the plan is deleted *in the merge*. It is still present; delete it as the last
  commit before the PR, not before.

---

## 8. NEXT STEPS

1. **Run `go run ./cmd/memqllint dsl/`.** Expect "308 file(s) loaded, no
   diagnostics." If that fails, something regressed; fix it before anything else.
2. **Run `make test > /tmp/run.log 2>&1`** (it takes ~15 min). Get the real
   failing set — §2's list is from a partial run.
3. **Fix the remaining fixtures**, in this order — they are all one of four
   known shapes, and §2 names the tests:
   - origin-less `AuthorSessionBundle` / `ValidateBundle` / `PromoteBundleDurable`
     calls → pass a tree-relative path like `"trainingns/concepts.memql"`
   - `mutate` in a detector regex or an expectation string → `mutation`
   - `;` as AND in a DSL fixture string → `&&` (**one file at a time**, never a
     repo-wide regex)
   - a fixture that lost `@enabled` / `@namespace` and now has the wrong line
     number or annotation count
4. **Confirm one clean `make test`**, with only the three `scripts/*` packages
   red. Compare against that exact list — do not claim green.
5. **Delete the plan**: `git rm docs/superpowers/plans/2026-09-14-dsl-v1-attributes.md`
   (the epic requires it to go in the merge).
6. **Post the verification answers on GitHub** — issue #5378's acceptance
   criterion is literally "the epic issue carries the reader list or the word
   none, with file references". Post the `@displayCard` / `@composable` readers
   on #5378, and the `@allowedRoles` finding on #5377.
7. **Push and open ONE PR** for #5375 #5376 #5377 #5378 #5379 (the user was
   explicit: one PR, not one per issue). The body must call out:
   the wire-contract change (§6.1), the four kept annotations (§3), and the
   `,`-as-OR deferral to #5363.
8. **Merge via the queue**: `gh pr merge <n> --repo znasllc-io/memql` — bare, no
   flags. `--delete-branch` is refused and `--merge` is ignored. It **enqueues**;
   a 5-minute batch window means it sits at OPEN with `mergedAt: null` for
   minutes with nothing wrong. If it goes `DIRTY`, rebase on `origin/main` and
   force-push. To merge as the owner:
   `scripts/dev/merge-as-owner.sh --pr=<n> --check` first, then without `--check`.
9. **Close #5375–#5379** once merged.
