# Materializer App and Files New Markdown Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rebuild the MemQL OS Materializer as the shell's one wizard, Make a file (kind, what to make it from, a description, name, folder), with a page per made file, Materialized / Templates / Recipes as its tabs, and add New Markdown file to the Files Add menu, opening that wizard with Markdown and the current folder answered.

**Architecture:** Task 1a first moves the described rule (spec D31): the server (`integrations/compose`) decides a request's path from the picked files' rows before it reads any content, and sends a described Markdown request to the composer whenever a picked file is one the Library cannot attach; the SDK applies the same rule (`isDescribedMarkdown` over file facts, `readDescribedMarkdown` reading the rows), and the Library's drift guard holds both copies. Everything after it is the OS client. One pure model (`apps/materializer/make.ts`) decides the steps, their answers, what the floor says and offers, and the request each draft sends; `MakeWizard` draws it on the kit `Wizard` and turns into the making stage after the press; a made file opens on `CompositionPage` with one `ActionBar` computed by a pure `pageActs`. Creation still goes only through the SDK's `startMaterialize`, the description hand-off is sub-project 1's, and the starting-setup contract (`items/materialize.ts`) is unchanged except for one added constant. No wire shape changes. Generated files: the typed SDKs after one DSL description edit (Task 1a), and the OS navigation catalogue and platform graph (Tasks 9 and 10).

**Tech Stack:** Go (`integrations/compose`, `integrations/library`'s drift guard; unit tests over fakes, DB-gated tests over the test Postgres), the TS SDK (`sdk/ts`, node:test; `@znasllc-io/memql-sdk-core/client`: `startMaterialize`, `isDescribedMarkdown`, `readDescribedMarkdown`, `libraryFileFacts`, `LibraryFileLookup`, `handOffDescription`, `fileNameProblem`, `fileNameStem`, `refusalSentence`, `sameEntityId`), the VS Code productivity extension's suite as a check (`editors/productivity`), the React OS client (`clients/os`, vitest + Testing Library), the OS kit (`clients/os/src/kit`), the browser QA harness (`clients/os/qa`).

**Spec:** `docs/superpowers/specs/2026-10-12-materializer-app-design.md`. Read it first; its "Words on screen" table is the copy contract. Also binding: `docs/superpowers/specs/2026-10-11-materializer-front-door-design.md` section 4 and amendments 5 to 11.

**Tracking:** sub-projects #6034 (the app, Tasks 1, 1a, 2-11, 13-17) and #6035 (Files, Task 12) of epic #6022. Branch `epic/materializer-app`.

## Global Constraints

- Fresh worktree, once: `make identity-tailwind`; `cd sdk/ts && npm ci && npm run build`; `cd clients/os && npm ci`. Build `sdk/ts` before any `clients/os` typecheck or test, and rebuild it (`npm run build`) after every SDK change: the OS and the VS Code extension consume its build. Disk is tight: never run two `npm ci` at once, and remove scratch `node_modules` when done (`editors/productivity/node_modules` once Task 1a's check has passed).
- Where tests live: OS tests only under `clients/os/test`; Go tests beside their package; SDK tests in `sdk/ts/test`; the VS Code extension's in `editors/productivity/test`. Per OS task: `cd clients/os && npm run typecheck && npx vitest run <the task's paths>`; a task that touches a shared piece (kit, items, Files, registry) also runs `npx vitest run` (the whole suite).
- Every `go test` command sets `MEMQL_DATABASE_DSN`, as each task writes it out. Unit and root runs use a closed port, `MEMQL_DATABASE_DSN='postgres://x:x@127.0.0.1:1/x?sslmode=disable'`, so DB-gated tests skip at once. DB-gated runs use a fresh scratch database on the test Postgres at `localhost:55433` (user `memql`, password `memql_dev`; `createdb` and `dropdb` are in `/opt/homebrew/bin`), created by the task before the run and dropped after it, with `MEMQL_REQUIRE_DB=1`. Never `localhost:5432`: that is the owner's local cluster's database. If nothing answers on 55433, stop and report; do not start a database. Always `-count=1`: the drift guard reads TypeScript, which the Go test cache does not track. Stage a new Go file (`git add <path>`) before the root tests run, because the origin gates read the git index.
- The SDK is hand-written except its `generated_*` files. After a DSL edit, `make sdk-gen` regenerates them and `make sdk-gen-check` must pass.
- Sub-project 1's guard stays green in every task that touches creation (Tasks 1a, 5, 6, 7, 12, 13): `cd clients/os && npx vitest run test/onePathCreate.test.ts test/files/onePath.test.ts`. Nothing under `clients/os/src` names `composeMaterialize`, `createLibraryFile`, `createArtifact`, `createGeneratedOutput`, or builds `new File(`.
- The starting-setup contract is unchanged: `MATERIALIZER_APP_ID`, `MATERIALIZER_COMPOSE_SECTION = "composer"`, `composeIntent`, `readComposeIntent`, and the keys `format`, `folderId`, `sources`, `outputKind`, `returnTo`. Additions only (`MATERIALIZER_MADE_SECTION`, Task 10).
- Bare ids: pass an id as the cluster gave it; compare with the SDK's `sameEntityId`; never split, compose or parse a canonical id (`idTail` is deleted in Task 4).
- The interface language binds: `clients/os/DESIGN.md` (the twelve rules, "One wizard for adding", "Record lists", "Loading is the shape of the content", "One trail row"), `clients/os/SUPERVISED-VISUAL-COMPOSITION.md`, `.agents/skills/memql-ui-design/SKILL.md`. An act that is not legal is absent, never disabled; acts that change a record live only in its `ActionBar`, at most three, primary last, one button, ways out as `Act.text`; a list and its page never share a scroller; loading is a skeleton, never a caption.
- Copy: the spec's "Words on screen" strings are the contract and tests assert them. Sentence case, plain verbs, no emojis, no ALL-CAPS labels, no developer voice. A refusal is the cluster's sentence through `refusalSentence`, verbatim, in place.
- Git: stage by explicit path (`git add <path>`, `git mv` for moves), never `-A` or `.`. Write each commit message to a file in your scratchpad and `git commit -F <file>`; end it with the Co-Authored-By trailer your session's attribution rule gives (for this program `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>` unless your session says otherwise). Do not push unless told. If `git commit` reports `index.lock`, wait a few seconds and retry.
- memql is a public repository: fixtures, scenes and screenshots use neutral names (Acme, Q3 summary, Meeting notes), never a client's names, copy or brand.
- Do not run docker, touch a cluster or deploy, except Task 17, which the controller runs after asking the owner. Screenshots are written under the scratchpad, never the repository.
- When a section list or a section's name changes (Tasks 9 and 10): `cd clients/os && MEMQL_UPDATE_NAVIGATION=1 npx vitest run test/ask/navigationContract.test.ts`, then `make platform-graph && MEMQL_DATABASE_DSN='postgres://x:x@127.0.0.1:1/x?sslmode=disable' go test -count=1 -run TestPlatformGraph .`; commit `component/memql/os_navigation.json` and `component/architecture/embedded/platform.graph.json`. Both record apps, sections and their names, not module needs. Never hand-merge either: regenerate after any merge.
- `clients/os/qa` is not type-checked by `npm run typecheck`. When a task touches `qa/` or moves a module it imports: `cd clients/os && npx vite build --config qa/vite.config.ts --outDir "$SCRATCH/qa-build" --emptyOutDir` (`$SCRATCH` is your scratchpad).
- If `clients/os/test/readTheBar.ts` exists on the branch (the email work has merged), a test that presses an act its bar has just changed awaits `readTheBar()` first, as the email suite does; otherwise it presses directly. Task 1 records which.
- App-local styles go in `clients/os/src/apps/materializer/materializer.css` with the class prefix `mz-`; `styles/index.css` is touched only in Task 13.

## Shared with email/os

The parallel plan `docs/superpowers/plans/2026-10-11-email-app.md` (branch `email/os`) touches these files too:

- **Identical change, by cherry-pick (Task 1):** email/os commit `e78b0ac8c` (kit `TextArea`, `DateTimeInput`, `Subhead` `actions`, `LiveList` `errorContent` and optional `emptyText`): `clients/os/DESIGN.md`, `clients/os/src/kit/controls.tsx`, `clients/os/src/kit/index.tsx`, `clients/os/src/live/LiveList.tsx`, `clients/os/src/styles/index.css`, `clients/os/src/apps/fleet/routing/DescribeRulePage.tsx`, `clients/os/src/apps/fleet/routing/routing.css`, `clients/os/src/apps/logs/SearchSection.tsx`, `clients/os/src/modules/connections/GitHubAccountsSettings.tsx`, `clients/os/src/modules/connections/AzureEmailConnections.tsx`, `clients/os/test/kit/{dateTimeInput,liveListStates,subhead,textArea}.test.tsx`. No later email/os commit edits these regions, so the two branches merge cleanly.
- **Edited independently:** `clients/os/src/styles/index.css` (this plan deletes the `.os-mz-*` block and `.os-mz` in the `.os-window-content:has(...)` selector; email/os edits other blocks); `clients/os/src/apps/registry.tsx` (the materializer manifest only); `clients/os/test/system/readinessMapping.test.ts` (the Materializer case; email/os edits the Campaigns case above it); `clients/os/qa/main.tsx` (one import and one spread line); `clients/os/qa/README.md`; `clients/os/README.md` (Materializer and Files sections only); `docs/public/operate/memql-os.md` (the "### Materializer" section only; its app-table row is left alone because email/os rewrites the neighbouring Campaigns row); `component/memql/os_navigation.json` and `component/architecture/embedded/platform.graph.json` (regenerate after merging).
- **Not edited here, affected on merge:** `clients/os/test/onePathCreate.test.ts` lists `apps/campaigns/AudiencesSection.tsx`, which email/os moves into `apps/email/`; that branch moves the entry. `clients/os/src/items/editorPreference.ts` gains email/os's `new-email` resource; this plan uses `editorArtifactURL` unchanged. New Files tests go in new files (`test/files/newMarkdown.test.tsx`, `test/files/folderSelect.test.tsx`) because email/os edits `test/files/browse.test.tsx`.
- **Task 1a's files and the other email branches:** `sdk/ts/CHANGELOG.md` gains entries on email/os too; edit only the Unreleased materialize entry and merge the two by hand, keeping both. Today no email branch edits `integrations/compose`, `sdk/ts/src/client/materialize.ts`, `dsl/compose/builtins.memql` or `integrations/library/review_attachments_sdk_test.go`, but the email layouts and VS Code plans (branches `email/layouts`, `email/vscode`) will add a layout to a composition through them. Whichever branch merges second reruns Task 1a's Go, SDK and drift-guard commands after the merge, and `make sdk-gen` when both changed the DSL.
- **Consistency:** email/os sends no Materializer starting setup ("Create new template" opens VS Code's New email page, its Ruling 9). New Markdown file is the only sender; `outputKind: "email_template"` stays honoured for a future one (spec D8).

## Review Focus

1. **A double press of Make the file** (two clicks, or Enter then a click) makes one file: one `composeMaterialize`, one request id, and the making stage from the first reply, before the feed has the row. Test in Task 6.
2. **A folder the person cannot use** (a setup's folder archived meanwhile) is sent as claimed, the refusal "That folder isn't available. Choose another folder." shows under "The file was not made.", the folder stays selected as "Unavailable folder", and choosing another folder is a new ask that succeeds. Test in Task 6.
3. **Stop racing the file being made** (`composeCancel` answers `cancelled: false`): the page reads Ready, the file is kept, nothing opens by itself, and a described file's description is not sent ("Stopped before the description was sent."). Tests in Tasks 6 and 7.
4. **The reserved editor tab never lingers or misfires:** it closes on a failure, a Stop or a refused start; it is never reserved for VS Code, Cursor or a file a model writes. Test in Task 7.
5. **A described Markdown file with a picked CSV goes to the composer**, which reads the CSV: the request carries `content: true` on it, the floor says "A model writes it.", nothing is handed to the Library, and no editor tab is reserved. The server decides the path before it reads any content: a described request with a picked Markdown file reads none, even on a node with no reference reader. Tests in Tasks 1a, 2, 5, 6 and 7.

---

### Task 1: Start the branch on the shared kit

**Goal:** The branch is current with main and carries the kit's `TextArea` through the same commit email/os made.

**Files:** none of this plan's own; the cherry-pick brings the files listed under "Shared with email/os".

- [ ] **Step 1: Bring the branch up to date.** `git fetch origin && git merge --no-edit origin/main`.
- [ ] **Step 2: Bring the kit's `TextArea` if main lacks it.** `grep -n "export function TextArea" clients/os/src/kit/controls.tsx || git cherry-pick -x e78b0ac8c`. Cherry-pick, never re-implement: the identical change is what lets this branch and email/os merge cleanly. It applies cleanly onto `386e5af39` (main's edits since email/os's base touch `index.css` only at lines 11162-11198; the commit's hunks are at 2189 and 14014).
- [ ] **Step 3: Set up the worktree** (Global Constraints, first line).
- [ ] **Step 4: Baseline.** `cd clients/os && npm run typecheck && npx vitest run test/kit test/logs test/fleet test/settings test/materializer test/files test/onePathCreate.test.ts` → PASS.
- [ ] **Step 5: Record** in your task report whether `clients/os/test/readTheBar.ts` exists. No commit of your own (the cherry-pick makes one).

---

### Task 1a: Mixed files go to the composer (server and SDK)

**Goal:** A described Markdown request takes the Library revision path only when every picked Library file is one the Library can attach, or nothing is picked; any other picked file sends it to the composer, which reads it. The server decides the path before it reads any content, and reads content only on the composed path (spec 2.10, D13, D31).

**Files:**
- Create: `integrations/compose/described.go`, `integrations/compose/described_test.go`
- Modify: `integrations/compose/materialize.go` (`describedMarkdown` takes the resolved sources; `materialize` decides the path before `captureSourceContents` and calls it only on the composed path; `onlyLibraryFiles` is deleted; the comments say the new rule), `integrations/compose/materialize_test.go`, `integrations/compose/materialize_db_test.go`
- Modify: `integrations/library/review_attachments_sdk_test.go` (the drift guard holds compose's copy and the SDK's predicate as well; its header says so)
- Modify: `dsl/compose/builtins.memql` (the `statement` argument's description); regenerate `sdk/go/client/generated_builtins.go` and `sdk/ts/src/client/generated_builtins.ts` with `make sdk-gen`
- Modify: `sdk/ts/src/client/materialize.ts` (the header's "A DATA SOURCE KEEPS THE COMPOSER" and "REFERENCES THE LIBRARY CANNOT ATTACH" paragraphs, `isDescribedMarkdown`, `startMaterialize`'s id check, `materialize`, and the hand-off's `lookUp`, which reads a row through `libraryFileFacts`), `sdk/ts/test/materialize.test.ts`, `sdk/ts/CHANGELOG.md` (the Unreleased materialize entry only). The barrel already re-exports the module (`export * from "./materialize.js"`).
- Modify: `clients/os/src/apps/materializer/actions.ts` (decides with `readDescribedMarkdown`), `clients/os/test/materializer/startingSetup.test.tsx`
- Check, unchanged: `editors/productivity` (its New Markdown sends a blank request, which reads no file; its email examples send `outputKind`, which cannot be described)

**Interfaces:**
- Go, in `integrations/compose/described.go` (compose may not import `integrations/library`: the area graph forbids it, so this is a copy the Library's drift guard holds to its constants). Write each declaration on its own line at the top level, as the Library writes its own:

```go
const libraryAttachMarkdownBytes = 32 << 10                            // the Library's reviewMarkdownBytes
const libraryAttachImageBytes = 8 << 20                                // the Library's reviewImageBytes
var libraryAttachImageFormats = []string{"png", "jpeg", "gif", "webp"} // reviewImageFormats; ".jpg" is read as ".jpeg"

// attachableByLibrary: the Library would take a file of this name and size as a review
// attachment. The extension is path.Ext of the name, lower-cased; ".md" up to
// libraryAttachMarkdownBytes, an image format up to libraryAttachImageBytes. A size of
// 0 is not attachable; sized=false (the row says no size) is not held against it.
func attachableByLibrary(name string, size int64, sized bool) bool
// rowSize: a file row's "size" when it is a JSON number (int, int64, float64, json.Number).
func rowSize(row map[string]any) (size int64, sized bool)
// attachableSource: a library_file source that resolved to exactly one row, with no
// problem, not archived, whose name and size attachableByLibrary takes.
func attachableSource(r Resolved) bool
```

- Go, in `materialize.go`: `func (a materializeArgs) describedMarkdown(resolved []Resolved) bool` is the old shape test (Markdown, a statement, no draft, section keys, template, recipe, output kind or deployable kind) and every resolved source `attachableSource`. In `materialize`, after `resolve`: `described := !fromARun(ctx) && a.describedMarkdown(resolved)`; `captureSourceContents` runs only when `!described`; `request.Described = described`.
- SDK, in `sdk/ts/src/client/materialize.ts`:

```ts
/** What decides whether the Library can attach a picked file: its file row's name, its size (null when the row says none) and whether it is archived. */
export interface LibraryFileFacts { name: string; size: number | null; archived: boolean }
/** The facts of the file a library_file source names, by its ref as given: null when its row cannot be read, undefined when the caller does not know. */
export type LibraryFileLookup = (fileId: string) => LibraryFileFacts | null | undefined;
/** A file row (libraryFileById, libraryFilesForOwner) as facts: size is a finite number or null; null for no row. */
export function libraryFileFacts(row: Record<string, unknown> | null | undefined): LibraryFileFacts | null;
/** Mirrors describedMarkdown in integrations/compose: the shape test, and every source a library_file whose facts are known, not archived, and taken by attachmentKind. */
export function isDescribedMarkdown(request: Pick<MaterializeRequest, "format" | "statement" | "templateId" | "recipeId" | "outputKind" | "deployableKind" | "sources">, files: LibraryFileLookup): boolean;
/** isDescribedMarkdown with the facts read: one libraryFileById per distinct picked file `known` does not answer for; nothing at all for a request whose shape cannot be described. A refused read is thrown as the cluster's sentence (inPlainWords). */
export function readDescribedMarkdown(execute: MaterializeExecute, request: Pick<MaterializeRequest, "format" | "statement" | "templateId" | "recipeId" | "outputKind" | "deployableKind" | "sources">, known?: LibraryFileLookup): Promise<boolean>;
```

  `startMaterialize` still refuses an id the hand-off cannot carry for any request whose SHAPE could be described (an unexported `describable(request)`), reading nothing. `materialize` reads first: `readDescribedMarkdown`, then starts nothing if the signal was aborted meanwhile, then `startMaterialize`, the follow, and the hand-off only when the read said described. `isDescribedMarkdown` calls `attachmentKind` in its own body (the drift guard checks this).
- The `statement` description in `dsl/compose/builtins.memql` becomes, verbatim: "What you asked for, in your own words. Becomes the goal's statement, so it is what Nexus shows and what the catalog keys on. A Markdown file asked for with a statement and nothing of its own to compose from (no draft, sectionKeys, templateId, recipeId, outputKind or deployableKind), whose sources, if any, are all Library files the Library can attach to a review comment (a .md file up to 32 KiB, or a PNG, JPEG, GIF or WebP picture up to 8 MiB), is created EMPTY outside a work run, and the statement stays on its composition: the caller hands it, with those files, to the Library's revision workflow, as the person (the SDK's handOffDescription), which writes the document. Any other source (a record, a query, or a Library file the Library cannot attach) keeps the composer, which reads it. Inside a work run the composer still writes the file from the statement."

- [ ] **Step 1: Write the failing Go tests.**
  - `described_test.go`: `TestAttachableByLibraryIsTheLibrarysPerFileRule`, a table: taken: `notes.md` of 1 byte and of exactly 32768, `NOTES.MD`, `notes.md` with no size, `logo.png` of exactly 8388608, `photo.jpg`, `photo.jpeg`, `anim.gif`, `pic.webp`; not taken: `notes.md` of 32769 or 0 bytes, `logo.png` of 8388609, `notes.markdown`, `notes.txt`, `numbers.csv`, `data.json`, `page.html`, `site.zip`, `vector.svg`, `spec.pdf`, `README`. `TestDescribedMarkdownNeedsEveryPickedFileToBeOneTheLibraryAttaches` over `describedArgs()` and resolved sources built by a small helper: described with none, one `.md`, a `.md` and a `.png`; not with a CSV, a `.md` and a CSV, a 40 KiB `.md`, an archived `.md`, a source whose row could not be read, a query, a concept row.
  - `materialize_test.go`: `TestDescribedMarkdownMeansAStatementAndNothingToComposeFrom` calls `describedMarkdown` with resolved sources (its Library-file cases use `.md` rows; its query, concept-row and mixed cases pass resolved sources of those kinds). Replace `TestDescribedMarkdownKeepsCapturingOptedInReferences` with `TestDescribedMarkdownReadsNoReferenceContent`: a `.md` source with `Content: true` is accepted on a node with no source downloader at all, and on one with a counting `referenceReader` it is read 0 times; the saved input holds none of the file's text; another replica with `forbidComposer` files "\n". Add `TestDescribedMarkdownWithAFileTheLibraryCannotAttachIsWrittenByTheComposer`, a table over `numbers.csv`, `data.json`, `notes.txt`, `page.html`, a `.md` whose row says 40960 bytes and an archived `.md`, each with `Content: true` and a `referenceReader` over its bytes (the row's `sha256` matching): read once at accept; the adopting replica's recording composer is asked once, with the statement, and its source's captured `Files[0].Text` holds the file's text; the uploaded bytes are the composed body, not "\n". Add `TestAPickedFileNobodyCanReadRefusesTheStart`: a described request naming a `library_file` with no row, `Content: true`: `materialize` returns an error saying the reference "is not readable by you", and no composition was written. `TestDescribedMarkdownWithLibrarySourcesStillFilesBlank`, `TestDescribedMarkdownFromDataSourcesStillUsesTheComposer` and `TestDescribedMarkdownInsideARunStillUsesTheComposer` stay as they are and must still pass.
  - `materialize_db_test.go`: give `materializeRequestDB` an options argument (Library files to seed under the owner through the accepting node's `store().createLibraryFile` and `setLibraryFileReady`, the calls the composer files with; a source downloader for the accepting node; a composer for the executing node), existing callers passing none. Add `TestMaterializeDB_DescribedMarkdownWithAMarkdownFileReadsNothingAndIsFiledBlank` (a ready `notes.md` of 2048 bytes, a counting reader that is never called, "\n" uploaded, the file named in the composition's sources) and `TestMaterializeDB_DescribedMarkdownWithACSVIsWrittenByTheComposer` (a ready `numbers.csv` whose `sha256` is that of the reader's bytes: read once at accept; the executing node's composer sees the CSV text; the uploaded file holds its draft).
  - `review_attachments_sdk_test.go`: `TestComposeMirrorsTheReviewAttachmentRules` parses `../compose/described.go` with `go/parser`, evaluates `libraryAttachMarkdownBytes` and `libraryAttachImageBytes` with `types.Eval` over `types.ExprString` of each value, and compares them with `reviewMarkdownBytes` and `reviewImageBytes`, and `libraryAttachImageFormats`'s string literals with `reviewImageFormats` as sets; a declaration that is missing fails with a sentence naming the file, as the SDK mirror's do.
- [ ] **Step 2: Run, verify failure.** `make identity-tailwind` (once per worktree), then `MEMQL_DATABASE_DSN='postgres://x:x@127.0.0.1:1/x?sslmode=disable' go test -count=1 ./integrations/compose/ ./integrations/library/` → FAIL (`attachableByLibrary` undefined; `describedMarkdown` called with an argument).
- [ ] **Step 3: Implement** `described.go` and the `materialize.go` change; rewrite the comments on `describedMarkdown` and above `request.Described` to say the rule and that the path is decided before any content is read; edit the DSL description; `make sdk-gen && make sdk-gen-check`.
- [ ] **Step 4: Run.**
  - `MEMQL_DATABASE_DSN='postgres://x:x@127.0.0.1:1/x?sslmode=disable' go test -count=1 ./integrations/compose/ ./integrations/library/ ./dsl/` → PASS.
  - The DB-gated cases, as ONE shell command, so the scratch database is dropped whether or not they pass:

```bash
DB="mz_task1a_$(date +%s)"; PGPASSWORD=memql_dev /opt/homebrew/bin/createdb -h localhost -p 55433 -U memql "$DB" && { MEMQL_DATABASE_DSN="postgres://memql:memql_dev@localhost:55433/$DB?sslmode=disable" MEMQL_REQUIRE_DB=1 go test -count=1 -run 'MaterializeDB' ./integrations/compose/; status=$?; PGPASSWORD=memql_dev /opt/homebrew/bin/dropdb -h localhost -p 55433 -U memql "$DB"; test $status -eq 0; }
```

  - `git add integrations/compose/described.go integrations/compose/described_test.go`, then `MEMQL_DATABASE_DSN='postgres://x:x@127.0.0.1:1/x?sslmode=disable' go test -count=1 .` → PASS.
- [ ] **Step 5: Commit** `integrations/compose/described.go integrations/compose/described_test.go integrations/compose/materialize.go integrations/compose/materialize_test.go integrations/compose/materialize_db_test.go integrations/library/review_attachments_sdk_test.go dsl/compose/builtins.memql sdk/go/client/generated_builtins.go sdk/ts/src/client/generated_builtins.ts`, and any other file `make sdk-gen` rewrote (`git status` names it), with subject `fix(compose): mixed files go to the composer, decided before contents are read (#6034)`.
- [ ] **Step 6: Write the failing SDK, drift-guard and OS tests.**
  - `sdk/ts/test/materialize.test.ts`: the `isDescribedMarkdown` cases take a lookup and run case for case with Step 1's Go table (taken: none picked, `notes.md` of 1200 bytes, exactly 32 KiB, `NOTES.MD`, no size, `logo.png` of exactly 8 MiB, `photo.jpg`, `anim.gif`, `pic.webp`; not: 32 KiB + 1, size 0, 8 MiB + 1, `.markdown`, `.txt`, `.csv`, `.json`, `.html`, `.zip`, `.pdf`, archived, a file the lookup does not know, a row that cannot be read, a query, a concept row, a `.md` beside a CSV). `test("readDescribedMarkdown reads each picked file's row once, and only those it is not told")` (`f-notes` listed twice and `f-logo` known: one `libraryFileById`, for `f-notes`); `test("readDescribedMarkdown reads nothing for a request that cannot be described")` (a template, a query among the files, no statement, `txt`: no calls); `test("a picked file with no row makes it a file the composer writes")`; `test("a refused read is the cluster's sentence, and materialize starts nothing")`; `test("materialize hands nothing on when a picked file is one the Library cannot attach")` (`described` plus `f-spec`, `spec.pdf`: `libraryFileById` before `composeMaterialize`, no comment, no revision, no `revisionId`); `test("a stop during the read starts nothing")`. Update "materialize on a described Markdown request returns the revision once the file is in Files" (the calls now begin `libraryFileById`, `composeMaterialize`, `compositionById`, `libraryArtifactForFile`) and "stopping after the file exists keeps the file, sends nothing, and says the description was not sent" (the file is read once, to decide the path, and not again: `libraryFileById` once, not zero; any other count of `libraryFileById` in a `materialize` test of a request with picked files gains that one read), and replace "materialize puts the references the Library could not take on its result" with the Markdown total: three `.md` references of 30 KiB each give `unattached: ["Third"]`. The barrel test lists `readDescribedMarkdown` and `libraryFileFacts`. The "materialize makes no Library hand-off call for ..." loop stays: none of those requests can be described, so none reads a file.
  - `review_attachments_sdk_test.go`: `TestSDKDecidesTheDescribedPathWithTheHandOffsRule`: the body of `export function isDescribedMarkdown` (from its line to the first line that is `}`) calls `attachmentKind(`, and the module exports `readDescribedMarkdown`.
  - `clients/os/test/materializer/startingSetup.test.tsx`: the three cases that relied on a PDF or a CSV being left off ("names each reference that went without being attached, on its own", "including the references that could not be attached, which arrive instead of a failure", "names the references that could not be attached, after opening the revision with the ones that could") are re-seeded on the Library's Markdown total: `.md` rows of 30 KiB each (`size: 30 * 1024`), four for the first case (two named) and three for the others (one named), with the attachments asserted in order. Add `it("does not hand on a Markdown request with a spreadsheet among its files: the composer reads it")` (a setup naming `notes.md` and `numbers.csv`, a description: one `libraryFileById` each before `composeMaterialize`; once the composition is ready, as the composer writes one, with a model recorded, no `libraryAddDocumentComment`).
- [ ] **Step 7: Run, verify failure.** `cd sdk/ts && npm test` → FAIL; `MEMQL_DATABASE_DSN='postgres://x:x@127.0.0.1:1/x?sslmode=disable' go test -count=1 -run TestSDKDecidesTheDescribedPathWithTheHandOffsRule ./integrations/library/` → FAIL.
- [ ] **Step 8: Implement** the SDK (the interfaces above; the header paragraphs say the new rule: a file the Library cannot attach keeps the composer, and only the Library's totals, or what only its bytes can show, leave a described file's reference unattached), the CHANGELOG entry (`isDescribedMarkdown(request, files)`, `readDescribedMarkdown`, `libraryFileFacts`, `LibraryFileLookup`; `materialize` reads the picked files' rows before it starts), and `actions.ts`: `start` awaits `readDescribedMarkdown(executeOver(query), request)` inside its `try`, before `startMaterialize`, and adds the file to `described` when it says so.
- [ ] **Step 9: Run.**
  - `cd sdk/ts && npm test && npm run build` → PASS.
  - `MEMQL_DATABASE_DSN='postgres://x:x@127.0.0.1:1/x?sslmode=disable' go test -count=1 ./integrations/library/` → PASS.
  - `cd editors/productivity && npm ci && npm run compile && npm test` → PASS unchanged; then `rm -rf editors/productivity/node_modules`, and `git status` shows nothing under `editors/`.
  - `cd clients/os && npm run typecheck && npx vitest run test/materializer test/onePathCreate.test.ts test/files/onePath.test.ts` → PASS.
- [ ] **Step 10: Commit** `sdk/ts/src/client/materialize.ts sdk/ts/test/materialize.test.ts sdk/ts/CHANGELOG.md integrations/library/review_attachments_sdk_test.go clients/os/src/apps/materializer/actions.ts clients/os/test/materializer/startingSetup.test.tsx` with subject `feat(sdk): a described file is decided from the picked files, as the server decides it (#6034)`.

---

### Task 2: The Make a file model

**Goal:** One pure module decides the wizard's steps, their answers, what the floor says and offers, and the request a draft sends.

**Files:**
- Create: `clients/os/src/apps/materializer/make.ts`
- Test: `clients/os/test/materializer/make.test.ts`

**Interfaces:**
- Consumes: `ComposeFormat`, `ComposeSetup`, `ComposeSource` (`items/materialize.ts`); `CompositionRow`, `SourceRow`, `TemplateRow` (`apps/materializer/rows.ts`); `formatWord`, `deployableWord` (`words.ts`); `MaterializeRequest`, `isDescribedMarkdown`, `LibraryFileLookup`, `fileNameProblem`, `fileNameStem` (SDK, Task 1a).
- Produces (later tasks import exactly these):

```ts
export type PackageKind = "spa" | "static" | "shopify_storefront";
export type FileKind = { kind: "document"; format: ComposeFormat } | { kind: "package"; deployableKind: PackageKind } | { kind: "emailTemplate" };
export interface Ingredient { kind: ComposeSource["kind"]; ref: string; label: string }
export interface MakeDraft {
  fileKind: FileKind; templateId: string; ingredients: Ingredient[]; statement: string;
  name: string; folderId: string; accountId: string;
  /** Optional steps the person (or a sender) has passed; passed with nothing in it reads as skipped. */
  passed: { from: boolean; describe: boolean };
}
export type MakeStepId = "kind" | "template" | "from" | "describe" | "name" | "folder" | "organization" | "file";
/** files: the facts of the person's Library files the window has read (Task 5's fileLookup); undefined for one it has not. */
export interface MakeFacts { templates: readonly TemplateRow[]; inference: boolean | null; files: LibraryFileLookup }
export type MakePath = "blank" | "described" | "composed";
export interface FloorWords { word: string; detail?: string }
export interface Blocker { reason: "name" | "blank" | "organization" | "inference"; words: FloorWords }
/** folder(id): the folder's name; null once the folder list has loaded without it; undefined while it loads. */
export interface AnswerNames { folder(id: string): string | null | undefined; template(id: string): string; account(id: string): string }
export type MakeAct = "cancel" | "next" | "make" | "setUpInference" | "stop" | "leave";

export const MAKE_STEPS: Record<MakeStepId, { name: string; sentence: string }>;   // spec 2.1, verbatim
export const FILE_KINDS: readonly { value: string; kind: FileKind; label: string; description?: string }[]; // spec 2.4 order
export function draftFromSetup(setup: ComposeSetup | null, defaultFormat: ComposeFormat): MakeDraft;
export function draftFromComposition(c: CompositionRow, sources: readonly SourceRow[]): MakeDraft;
export function formatOf(kind: FileKind): ComposeFormat;
export function stepsOf(draft: MakeDraft, facts: MakeFacts): MakeStepId[];
export function stepState(step: MakeStepId, draft: MakeDraft, facts: MakeFacts): "done" | "skipped" | "waiting";
export function openingStep(draft: MakeDraft, facts: MakeFacts): MakeStepId;
export function answerOf(step: MakeStepId, draft: MakeDraft, names: AnswerNames): string | undefined;
export function pathOf(draft: MakeDraft, facts: MakeFacts): MakePath;
export function blocker(draft: MakeDraft, facts: MakeFacts): Blocker | null;
export function readyWords(draft: MakeDraft, facts: MakeFacts): FloorWords;
export function floorActs(args: { draft: MakeDraft; facts: MakeFacts; open: MakeStepId; making: boolean; canSetUpInference: boolean }): MakeAct[];
export function requestOf(draft: MakeDraft): Omit<MaterializeRequest, "requestId">;
export function filedName(draft: MakeDraft): string;
export function ingredientsPhrase(ingredients: readonly Ingredient[]): string;
export function describeAnswer(statement: string): string;
export function readableReference(name: string): boolean;
```

Rules the bodies must follow (the spec decides; these are its values):
- `draftFromSetup(null, f)`: document `f`, everything empty, `passed` both false. With a setup: its format (or JSON plus `emailTemplate` for `outputKind`), its folder, its sources as ingredients, `passed` both true.
- `draftFromComposition`: kind from `format`/`deployableKind`/`outputKind`; `templateId`, `statement`, `name`, `folderId`, the first of `accountIds`; ingredients from the recorded sources; `passed` both true.
- `stepsOf`: `kind`, `template` (only when a non-archived template's format is the draft's format or empty), `from`, `describe`, `name`, `folder`, `organization` (only for `emailTemplate` or a non-empty `accountId`), `file`.
- `stepState`: `kind` and `folder` are always `done`; `template` `done`; `from`/`describe` `done` when they hold something, else `skipped` when passed, else `waiting`; `name` `done` when `fileNameProblem` is undefined, else `waiting`; `organization` `done` when set, `waiting` for an email template without one, `skipped` otherwise; `file` is drawn by the component.
- `openingStep`: the first `waiting` step in `stepsOf` order; `"describe"` when none waits.
- `answerOf`: kind, its `FILE_KINDS` label; template, its name or `"No template"`; from, `ingredientsPhrase` (undefined while `waiting`); describe, `describeAnswer` (undefined while `waiting`); name, `filedName` once usable; folder, `"Library"` for `""`, else the name, `"Unavailable folder"` for `null`, undefined while loading; organization, its name or `"No organization"`; file, undefined (the component shows `filedName` once made).
- `pathOf`: `blank` when no statement, no ingredients, no template and a document kind; `described` when `isDescribedMarkdown(requestOf(draft), facts.files)` (a file `facts.files` does not know counts as one the Library cannot attach, so the prediction never says described where the press, which reads the file, would not); else `composed`.
- `blocker`, first match: name unusable → `"Name the file"`; nothing to make it from (no statement, no ingredients) and (a template is chosen, or the kind is not a Markdown/text/HTML/CSV/JSON document) → `"Describe it or pick what to make it from"` with `"A template needs something to fill it."` when a template is chosen, else `"A blank file can be Markdown, text, HTML, CSV or JSON."`; an email template with no organization → `"Choose an organization"`; `facts.inference === false` and the path is not blank → `"Making this needs Inference"` (the component adds the detail by who can set it up).
- `readyWords`: `"Ready to make " + filedName(draft)` with detail, by `pathOf(draft, facts)`, `"It starts blank."` (blank), `"You review its draft in the editor."` (described), `"A model writes it."` (composed).
- `floorActs`: making → `["stop", "leave"]`; else `["cancel", F]` where F is `"make"` when `blocker` is null, `"setUpInference"` when the blocker is inference and `canSetUpInference`, `"next"` when some step after `open` in `stepsOf` order is `waiting`, else nothing.
- `requestOf`: the spec 2.10 table; omit every unanswered optional; every `library_file` ingredient carries `content: true` whatever the path (the server reads contents only on the composed path, spec D13).
- `filedName`: mirrors `outputFileName` in `integrations/compose/materialize.go:1148`: extension `md` for Markdown, `zip` for a package, otherwise the format; stem `fileNameStem(name, formatOf(kind))`; no second extension when the stem already ends with it.
- `readableReference`: extension in `.md .markdown .txt .html .htm .css .json .csv .svg .png .jpg .jpeg .gif .zip` (the composer's list, `integrations/compose/source_content.go`). Which of them a description can take is the SDK's to say (`isDescribedMarkdown`), never a second list here.
- `ingredientsPhrase`: 0 `"Nothing picked"`, 1 the label, 2 `"A and B"`, 3+ `"A, B and N more"`. `describeAnswer`: `""` → `"Not described"`, else the first line cut at a word within 60 characters plus `"…"` when cut.

- [ ] **Step 1: Write the failing tests** in `make.test.ts`, one `describe` per function above, asserting the exact values listed, including at least: a Files setup (`{format: "markdown", folderId: "f-projects", returnTo: {appId: "files", sectionId: "browse"}}`) gives `openingStep` `"name"` and `stepState("from")` `"skipped"`; `draftFromSetup(null, "markdown")` gives `openingStep` `"from"`; a PDF with only a name is blocked with the blank detail; `requestOf` of a blank Markdown draft named "Notes" equals exactly `{ name: "Notes", format: "markdown" }`; a described Markdown draft with `notes.md` (2 KiB) and `logo.png` picked, both known to `facts.files`, is `described`, says "You review its draft in the editor." and sends both with `content: true`; the same draft with `data.csv` added is `composed` and says "A model writes it."; so is it with a `notes.md` of 40 KiB, an archived one, one `facts.files` does not know, or a `query` ingredient; a static package sends `{ format: "html", deployableKind: "static" }` and is filed `"Site.zip"`; `filedName` of `"Plan.md"` as Markdown is `"Plan.md"`; `floorActs` never returns more than two acts and never `"make"` while `blocker` is non-null.
- [ ] **Step 2: Run, verify failure.** `cd clients/os && npx vitest run test/materializer/make.test.ts` → FAIL (module not found).
- [ ] **Step 3: Implement** `make.ts`.
- [ ] **Step 4: Run.** `cd clients/os && npm run typecheck && npx vitest run test/materializer/make.test.ts` → PASS.
- [ ] **Step 5: Commit** `make.ts` and its test with subject `feat(os): the Make a file model, one pure table (#6034)`.

---

### Task 3: Feeds that read every page, Library feeds on demand, and FolderSelect

**Goal:** The Materializer reads every page of its lists, reads the Library's files and folders only when a wizard or a page needs them, and Files and the Materializer share one folder picker.

**Files:**
- Modify: `clients/os/src/apps/materializer/useCompose.ts` (compositions, templates and recipes walk `result.meta()?.cursor` instead of `paged: false`; add `useLibraryFiles` over the person's file rows (spec D32), in the shape of Files' `files:files` feed in `apps/files/useLibrary.ts`: `libraryFilesForOwner`, concept `Concepts.LIBRARY_FILE` (`useTemplateFiles`, which reads the artifact index for the old Bind a file form, stays until Task 8 deletes it); add `useFolders`; `useCompositions` exposes the joined runs)
- Create: `clients/os/src/items/FolderSelect.tsx`
- Modify: `clients/os/src/apps/files/BrowseSection.tsx` (Move to folder draws `FolderSelect`; delete `flatFolderOptions`)
- Modify: `clients/os/test/materializer/harness.tsx` (`pagedResult(rows, cursor)`; seeds `libraryFiles` (file rows: `id`, `name`, `mimeType`, `size`, `status`, `archived`, `createdAt`), `folders`, `cancelReply`; `libraryFilesForOwner`, `libraryFolders` mocks that page)
- Test: `clients/os/test/materializer/feeds.test.tsx`, `clients/os/test/files/folderSelect.test.tsx`

**Interfaces:**
- Produces:

```ts
// useCompose.ts
export function useCompositions(): LiveCollectionHandle<Row> & { runOf(runId: string): Row | undefined };
/** The person's Library file rows (`libraryFilesForOwner`, every page): name, size, archived and the file's own id, which the tray offers and the routing reads; no read while `enabled` is false. */
export function useLibraryFiles(enabled: boolean): LiveCollectionHandle<Row>;
/** The person's live folders (`libraryFolders`, every page); no read while `enabled` is false. */
export function useFolders(enabled: boolean): LiveCollectionHandle<Row>;
// items/FolderSelect.tsx
export interface FolderOption { id: string; label: string }   // label indented two spaces per depth
export function folderOptions(folders: readonly { id: string; name: string; parentFolderId: string; archived: boolean; deleted: boolean }[]): FolderOption[];
export function FolderSelect(props: { id: string; label: string; value: string; onChange: (folderId: string) => void; options: readonly FolderOption[]; missingLabel?: string }): JSX.Element;
```

`folderOptions` sorts siblings by (name, id), drops archived and deleted folders, places a folder whose ancestry never reaches the top at the top, and terminates on a cycle (the rules of `apps/files/fold.ts`). `FolderSelect` is the kit `Select` with "Library (top level)" (value `""`) first; a `value` that is not among the options is kept as one more option labelled `missingLabel` (default `"Unavailable folder"`), never dropped.

- [ ] **Step 1: Write the failing tests.**
  - `feeds.test.tsx`: `it("lists every page of compositions")` (`compositions` answers 50 rows with cursor `"p2"`, then 3 rows with none: 53 rows render and the second call carries `cursor: "p2"`); the same for templates and recipes; `it("reads no Library files or folders until asked")` (`renderHook(() => useLibraryFiles(false))` makes no `libraryFilesForOwner` call; `true` walks two pages); the same for `useFolders`; `it("knows the run of an unfinished composition")` (`runOf("r1")` returns the seeded run).
  - `folderSelect.test.tsx`: indented order for a two-level tree; archived and deleted folders absent; an orphan at the top; a two-folder cycle terminates with both listed once; "Library (top level)" first; a missing value shows "Unavailable folder" and stays selected; Files' Move to folder still moves a row (`moveArtifactToFolder` called with the chosen id).
- [ ] **Step 2: Run, verify failure.** `cd clients/os && npx vitest run test/materializer/feeds.test.tsx test/files/folderSelect.test.tsx` → FAIL.
- [ ] **Step 3: Implement.** Keep each feed's key and reread as they are; a disabled feed passes a `null` key to `useLiveCollection`.
- [ ] **Step 4: Run.** `cd clients/os && npm run typecheck && npx vitest run test/materializer test/files` → PASS.
- [ ] **Step 5: Commit** the files above with subject `feat(os): Materializer feeds read every page; folders and Library files on demand (#6034)`.

---

### Task 4: A made file has its own page

**Goal:** A finished composition opens on a page of its own, in place of the list, with its facts and one bar.

**Files:**
- Rewrite: `clients/os/src/apps/materializer/acts.ts` (`pageActs`, `pageState`; the old `actsFor` and `stateLine` stay until Task 6 deletes the composer that uses them)
- Create: `clients/os/src/apps/materializer/CompositionPage.tsx`
- Modify: `clients/os/src/apps/materializer/MaterializedSection.tsx` (a row opens the page in place; the list stays mounted, hidden, with `navigation={false}` on its Head while a page shows), `MaterializerApp.tsx` (the `materialized` section renders list or page; page handlers; `useFolders` enabled while a page shows, for Filed in; delete `idTail`, compare with `sameEntityId`), `words.ts` (captions and the not-sent next line renamed to "Open it in the editor", spec "Words on screen")
- Test: `clients/os/test/materializer/pageActs.test.ts`, `clients/os/test/materializer/page.test.tsx`

**Interfaces:**
- Produces:

```ts
// acts.ts
export type PageAct = "back" | "archive" | "restore" | "saveRecipe" | "open" | "download" | "deploy" | "startOver";
export interface PageActSpec { id: PageAct; label: string; tone?: "primary"; text?: boolean }
export function pageActs(c: CompositionRow, facts: { backTo?: string; recipeCanRun: boolean; recipeSaved: boolean }): PageActSpec[];
export function pageState(c: CompositionRow): FloorWords;
// CompositionPage.tsx
export interface CompositionPageProps {
  composition: CompositionRow; sources: SourceRow[]; models: ModelRow[];
  filedIn: string | undefined;        // "Library", the folder's name or "Unavailable folder"; undefined while folders load
  templateName: string; organizationName: string;
  caption: string;                     // madeCaption(...)
  description?: { failure: string; unattached: string[] };
  notices: { editorSilent: boolean; recipeSaved: boolean };   // Task 7 adds tabRefusal: string
  acts: PageActSpec[]; busy: boolean; error: string;
  canOpenRun: boolean;
  onAct: (act: PageAct) => void; onOpenRun: () => void; onBack: () => void;
}
export function CompositionPage(props: CompositionPageProps): JSX.Element;
```

`pageActs` is spec 3.2's table: Ready `[Archive (text), Save as recipe (text, when recipeCanRun && !recipeSaved && no recipeId), Open in editor (primary)]`; with `backTo` `[Back to <app> (text), Save as recipe or else Archive (text), Open in editor]`; a package `[Archive or Back to <app> (text), Download (text), Deploy it (primary)]`; failed or cancelled `[Archive (text), Start over (primary)]`; archived `[Open in editor (text, when outputFileId), Restore (primary)]`; running `[]` (from Task 6 a running composition opens its making stage instead). `pageState`: the status word (`statusWord`: "Ready", "Failed" with the reason as detail, "Stopped", "Draft", "Composing", "Writing the file", "Unknown"), or "Archived" with "The file is still in Files." when the record is archived and has a file. The page is `.os-deploy-pane > .os-deploy-scroll > Panel > Head + Facts`, then the caption, the notices and sub-project 1's always-mounted live region, then the `ActionBar` (the `DeployablePage` shape). Facts as spec 3.1. Start over navigates to the composer for now; Task 6 fills it from the record.

- [ ] **Step 1: Write the failing tests.**
  - `pageActs.test.ts`: one case per row of spec 3.2's table (labels in order, primary last, at most three, `text` on every non-primary); `pageState` words.
  - `page.test.tsx`: `it("a row opens the file's page in place of the list, and Back returns to the list as it was")` (search kept; the list's rows hidden while the page shows); `it("states the facts, with Nothing and None as real answers")`; `it("names the folder it was filed in, or Unavailable folder")`; `it("offers Open the run only where Nexus can be opened")` (`openApp("nexus", "goals", { goalId })`); `it("saves a recipe once and says where it is")` (`createComposeRecipe` once; the act gone; "Saved as a recipe. It is in Recipes."); `it("archives the record and says the file stays")`; `it("restores an archived record")`; `it("a failure states its reason and offers Start over")`; `it("a package downloads and deploys")`; sub-project 1's caption cases (moved from `startingSetup.test.tsx` "the caption says what the file is", with the renamed words); `it("opens the file through the editor path, a ZIP downloading")` (moved from `app.test.tsx` "the provenance chain" opening cases).
- [ ] **Step 2: Run, verify failure.** `cd clients/os && npx vitest run test/materializer/pageActs.test.ts test/materializer/page.test.tsx` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run.** `cd clients/os && npm run typecheck && npx vitest run test/materializer` → PASS (update the old copies of the moved cases in `app.test.tsx`, `startingSetup.test.tsx` and `rows.test.ts` "the caption under what was asked for" to the new words, or delete them where moved).
- [ ] **Step 5: Commit** with subject `feat(os): a made file has its own page with one bar (#6034)`.

---

### Task 5: Make a file: the wizard's steps and the From tray

**Goal:** The Make a file wizard draws every step and the From tray, and offers Make the file the moment it is legal.

**Files:**
- Create: `clients/os/src/apps/materializer/MakeWizard.tsx`, `clients/os/src/apps/materializer/Ingredients.tsx`, `clients/os/src/apps/materializer/materializer.css`
- Test: `clients/os/test/materializer/wizard.test.tsx`, `clients/os/test/materializer/ingredients.test.tsx`

**Interfaces:**
- Consumes: Task 2's model; Task 3's `FolderOption`, `FolderSelect`; `useComposableConcepts`, `useResolvedSources` (`useCompose.ts`); the SDK's `libraryFileFacts`, `LibraryFileLookup`, `sameEntityId`; kit `Wizard`, `ChoiceStack`, `Input`, `TextArea`, `RecordList`, `RecordRow`, `Notice`, `RecordListSkeleton`, `IconButton`; `AccountPicker`.
- Produces:

```ts
// Ingredients.tsx
export interface LibraryFileOption { ref: string; title: string; mimeType: string; size: number | null; createdAt: string }
/** The person's Library file rows (Task 3's useLibraryFiles) that are not archived, newest first; `ref` is the row's own id as given, `title` its name. The tray offers those `readableReference(title)` takes; Add a template offers all but ZIPs (Task 8). */
export function fileOptions(rows: readonly Row[]): LibraryFileOption[];
/** MakeFacts.files over the same rows: the SDK's libraryFileFacts of the row whose id is sameEntityId with the ref, archived ones included; undefined when no row is. */
export function fileLookup(rows: readonly Row[]): LibraryFileLookup;
export function Ingredients(props: {
  picked: Ingredient[]; onPicked: (next: Ingredient[]) => void;
  files: { options: LibraryFileOption[]; state: LiveState; error: string };
  records: ComposablesState; counts: ResolveState;
}): JSX.Element;
// MakeWizard.tsx
export interface MakeWizardProps {
  draft: MakeDraft; onDraft: (next: MakeDraft) => void;
  facts: MakeFacts; names: AnswerNames;
  open: MakeStepId; onOpen: (step: MakeStepId) => void;
  files: { options: LibraryFileOption[]; state: LiveState; error: string };
  records: ComposablesState; counts: ResolveState;
  folders: { options: FolderOption[]; loaded: boolean };
  templates: TemplateRow[]; accounts: AccountRow[];
  refusal: string; starting: boolean;
  canSetUpInference: boolean; inferenceElsewhere: boolean;   // true: "An owner or developer can set it up."
  onMake: () => void; onCancel: () => void; onBack: () => void; onSetUpInference: () => void;
}
export function MakeWizard(props: MakeWizardProps): JSX.Element;
```

The wizard: icon lucide `FilePlus` (the orb names the thing being added), title "Make a file", rail label "Making a file", trail `Materialized > Make a file` with Back "Materialized" (`onBack`), `context={{ page: "Make a file", step: open }}`. Each step is a `Stop` with `MAKE_STEPS` name and sentence, `answerOf` as its answer and `stepState` (the open one `open`; `file` `ahead`). Bodies: Kind of file is a `ChoiceStack` (prose) over `FILE_KINDS` (Email template only when the draft is one); choosing sets the kind, drops an email template for a document, and opens `openingStep` of the new draft (or stays when nothing waits). Template is a `ChoiceStack` ("No template", then the usable templates). From is `Ingredients`. Describe is `TextArea` (`id="mz-describe"`, label "Describe what you want", 4 rows, the spec placeholder). Name is `Input` (`id="mz-name"`, label "Name", placeholder "Q3 summary", `onEnter`: make when `blocker` is null, else open the next waiting step), with `fileNameProblem` said beside it once typed (sub-project 1's `nameChecked` rule). Folder is `FolderSelect` (`id="mz-folder"`, label "Folder"). Organization is `AccountPicker` (required for an email template; "No organization" otherwise). Entering From or Describe marks it passed. The floor is `floorActs` with labels Cancel (text), Next, Make the file, Set up Inference, all `busy` while `starting`; the status is `blocker(draft, facts)?.words ?? readyWords(draft, facts)`, adding "An owner or developer can set it up." to the inference blocker when `inferenceElsewhere`. `refusal` draws `<Notice tone="error" sentence="The file was not made." detail={refusal} />` in the wizard's `notices`. Ingredients draws picked things as `.mz-card`s (glyph, name, fact, `IconButton` "Remove <name>"), the search (`Input` label "Search your files and records", placeholder "Search"), then Subheads "Files" (the options `readableReference` takes; 20 newest unless searching, then every match; "Search to find older files." when more) and "Records" (marked, then unmarked tagged "Unmarked"; none without a `list`), every row a `RecordRow` whose press toggles the pick and whose `selected` marks it; record cards show `counts` ("12 rows", an em dash, or the problem); states per spec 2.5. A card's rise is a CSS transition in `materializer.css`, none under `prefers-reduced-motion`.

- [ ] **Step 1: Write the failing tests** (render `MakeWizard` and `Ingredients` directly over `withSession`; the harness's connection for the hooks).
  - `wizard.test.tsx`: `it("a sender's setup opens on Name with the kind and the folder answered")` (the rail's Kind of file, From, Describe and Folder lines answer "Markdown", "Nothing picked", "Not described" and "Projects"; From and Describe carry the skipped mark; the Name field has focus; status "Name the file"; acts exactly Cancel); `it("the plus opens on From and moves on with Next")`; `it("offers Make the file the moment the name is usable, from any step")` ("Ready to make Meeting notes.md", "It starts blank.", one press → `onMake` once); `it("Enter in Name makes the file when it can")`; `it("a choice moves on, and a PDF with nothing to make it from says why")`; `it("asks for a template only when one makes this kind")`; `it("an email template asks for its organization")`; `it("names the file as it will be filed")`; `it("says what is wrong with the name once it is typed")` ("Enter a file name, without a folder path."); `it("keeps a folder it cannot see as Unavailable folder")`; `it("says the cluster's refusal over the steps")`; `it("when Inference is not set up, offers to set it up to those who can")` (owner: "Set up Inference" → `onSetUpInference`; others: the detail and no forward act); `it("a described Markdown file with a CSV says a model writes it")` ("Ready to make Notes.md" with "A model writes it."; with only `notes.md` picked instead, "You review its draft in the editor."); `it("draws no disabled act")` (no button under `.os-actbar` is `disabled` unless busy).
  - `ingredients.test.tsx`: `it("offers readable files and offered record types in one tray")` (`a.md` and `c.png` listed, `b.pdf` not; a record type without `list` absent; an unmarked one tagged "Unmarked"); `it("a press puts a thing in and takes it out")` (`onPicked` with `{ kind: "library_file", ref: "file-a", label: "a.md" }`, the file row's own id, then `[]`; the card's "Remove a.md"); `it("knows each file's facts by its id")` (`fileLookup` answers `{ name: "a.md", size: 2048, archived: false }` for `"file-a"`, the facts of an archived row too, and undefined for an id it has no row for); `it("search narrows both groups")`; `it("shows the twenty newest and says to search for older ones")` (25 files); `it("a record card counts its rows or says its problem")`; `it("loading is a skeleton, and an empty tray says what to do")` ("Nothing to pick yet. Files you upload in Files appear here."); `it("says when this node cannot read the record types")`.
- [ ] **Step 2: Run, verify failure.** `cd clients/os && npx vitest run test/materializer/wizard.test.tsx test/materializer/ingredients.test.tsx` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run.** `cd clients/os && npm run typecheck && npx vitest run test/materializer test/onePathCreate.test.ts test/files/onePath.test.ts` → PASS.
- [ ] **Step 5: Commit** with subject `feat(os): Make a file, the wizard's steps and the From tray (#6034)`.

---

### Task 6: Making a file in the wizard

**Goal:** Make the file starts one file, the same wizard shows it being made with Stop and Leave, and the finished file's page takes over; the old composer is gone.

**Files:**
- Modify: `clients/os/src/apps/materializer/MaterializerApp.tsx` (the `composer` section draws `MakeWizard`; the draft, its origin, the open step, the making composition id and the stopped set live at the root; `useLibraryFiles` and `useFolders` are enabled while the `composer` section shows; setups, the Compose tab and Start over make drafts; the page hand-over), `actions.ts`, `MakeWizard.tsx` (the making stage), `descriptionHandOff.ts` (a stopped composition is not handed on)
- Delete: `clients/os/src/apps/materializer/ComposerSection.tsx`, `actsFor` and `stateLine` in `acts.ts`
- Test: `clients/os/test/materializer/making.test.tsx`; migrate then delete `clients/os/test/materializer/startingSetup.test.tsx` and `clients/os/test/materializer/acts.test.ts` (its way-back and Save-as-recipe cases become `pageActs.test.ts` cases), and move `rows.test.ts`'s "acts follow the state" block to `pageActs.test.ts`; update `intent.test.tsx`

**Interfaces:**
- Consumes: Tasks 1a-5 (`readDescribedMarkdown`, `LibraryFileLookup`, `fileLookup`).
- Produces:

```ts
// actions.ts
export interface MakeStart extends WriteState {
  /** Described Markdown files this window started (for the hand-off), as before. */
  described: DescribedFile[];
  /**
   * Starts the file; resolves to the composition id, or "" after a refusal (then `error` holds the cluster's sentence).
   * It first decides whether the file is described with the SDK's readDescribedMarkdown over `files`, which reads only
   * the picked files `files` does not know (Task 1a); a refused read is the start's refusal. One start in flight; the
   * request id rules of sub-project 1 are unchanged.
   */
  start(request: Omit<MaterializeRequest, "requestId">, files: LibraryFileLookup): Promise<string>;
}
export function useMaterialize(): MakeStart;           // replaces the facts-taking hook; the organization check moves to the model
export type StopOutcome = "stopped" | "finished" | "refused";
// useCompositionActs().stop(compositionId): Promise<StopOutcome>   ("finished" when the reply says cancelled: false)
// descriptionHandOff.ts
export function useDescriptionHandOff(described: DescribedFile[], compositions: Row[], stopped: ReadonlySet<string>): DescriptionOutcome[];
// a stopped composition's outcome is { failure: "Stopped before the description was sent.", unattached: [] }, with no Library call
// MakeWizard.tsx, added props
making?: { composition: CompositionRow | null; since: string };   // since: the run's startedAt, else the press time (RFC 3339)
onStop: () => void; onLeave: () => void;
```

The root's `facts.files` is `fileLookup(libraryFiles.snapshot.rows)` (Task 5), and the press passes it to `start`. The making stage: every person's step shows its answer and does not open (`openable: false`); `file` is the open step, with state `current` and the file card (glyph, filed name, folder) as its body; status `{ word: "Making the file", detail: "Starting" | "Composing" | "Writing the file", tone: "busy", meta: m:ss since `since` }` (`useNow(1000)`); acts Stop (text) and Leave (button). The root switches the wizard into this stage on the id `start` returns, before the feed has the row. When the composition becomes ready, failed or cancelled and the window is still on this making stage, the root navigates to `materialized` with that composition's page open; otherwise it moves nothing. Cancel, Leave and the trail's Back discard the draft; Cancel and Leave then `openApp(returnTo.appId, returnTo.sectionId)` when the draft came from a setup whose app the shell holds and the person may open (`canOpen`), else `navigate("materialized")`. Stop calls `stop(id)`; `"finished"` adds the id to `stopped`. A setup draft's `returnTo` becomes the made composition's way back (sub-project 1's `returnFor`, kept for that composition only), which the page offers as Back to <app>. A draft for an email template starts with `useDefaultOrganization(accounts)` as its organization when there is one (the model is pure, so the root fills it). Start over: `draftFromComposition` of the page's composition, opened on `openingStep`, no `returnTo`. A setup arriving while a draft is open replaces it. A `{compositionId}` intent for a composition still being made opens its making stage in `composer`. Until Task 10 the Compose tab opens a fresh `draftFromSetup(null, defaultFormat)`.

- [ ] **Step 1: Write the failing tests** in `making.test.tsx`, mounting `MaterializerApp` over the harness with `useOsIfPresent` stubbed as `startingSetup.test.tsx` does:
  - `it("a double press starts one file and shows the making stage at once")` (two presses: one `composeMaterialize`, one request id matching `^[A-Za-z0-9_-]{8,111}$`; "Making the file", "Starting", acts Stop and Leave, before any composition row is emitted).
  - `it("follows the composition on the live feed and hands over to its page")` (emit `composing`, then `rendering`, then `ready`: details "Composing", "Writing the file", then `navigate("materialized")` and the page's "Ready").
  - `it("moves no window the person has taken elsewhere")`.
  - `it("Stop before the file exists stops it")` (`composeCancel({ compositionId, reason: "Stopped by the person" })`; a cancelled row; the page "Stopped").
  - `it("Stop after the file was made keeps it and sends no description")` (`cancelReply: { cancelled: false }`; a described draft; no `libraryAddDocumentComment`; the page "Ready" and "Stopped before the description was sent.").
  - `it("Leave goes back to the sender and the making goes on")` (`openApp("files", "browse")`).
  - `it("Cancel discards the draft and goes back to the sender")`.
  - `it("a row still being made opens its making stage")` (intent `{ compositionId }`, status `composing`).
  - `it("sends a setup's folder as claimed, says its refusal and keeps the same ask")` (`writeError` "That folder isn't available. Choose another folder."; "The file was not made." with that detail; a second press reuses the request id; no `libraryFolderById` read).
  - `it("a different folder is a new ask")` (choose another folder after the refusal: a new request id; the start succeeds).
  - `it("Start over fills the wizard from the record and opens on Describe")`.
  - `it("a described Markdown file with a CSV is written by a model and hands nothing on")` (`notes.md` and `numbers.csv` picked in the tray and a description: the floor says "A model writes it."; both sources are sent with `content: true`; once ready, no `libraryAddDocumentComment`).
  - `it("a described Markdown file with Markdown and a picture hands both on")` (`notes.md` and `logo.png`: "You review its draft in the editor."; no `libraryFileById` read before the start, because the tray's rows already know both; once ready and entered, the comment attaches both).
  - Migrated from `startingSetup.test.tsx`, rewritten for the wizard: the setup opens pre-filled (format, sources, folder, email template kind with JSON); a setup over a draft starts a new one; the section a window opens on; request ids (same ask after a refusal, new ask after a change, new ask after a composition); the whole "a Markdown file asked for by describing it" block (hand-off once when ready and entered; waits for the entry; ignores other files' entries; failure notice; unattached notices, on the Library's Markdown total as Task 1a seeded them; not for a spreadsheet, a template, records or a file the Library cannot attach; not for a composition this window did not start); the "returning to the app that sent the person" block, now on the page.
- [ ] **Step 2: Run, verify failure.** `cd clients/os && npx vitest run test/materializer/making.test.tsx` → FAIL.
- [ ] **Step 3: Implement**, then delete `ComposerSection.tsx`, the old acts and `startingSetup.test.tsx` once every case above has its new home (name each in the commit body).
- [ ] **Step 4: Run.** `cd clients/os && npm run typecheck && npx vitest run test/materializer test/onePathCreate.test.ts test/files` → PASS.
- [ ] **Step 5: Commit** with subject `feat(os): making a file in the wizard, with Stop and Leave (#6034)`.

---

### Task 7: A file filed blank opens in the editor by itself

**Goal:** A file the cluster files blank opens in the browser editor tab its press reserved; nothing else opens by itself.

**Files:**
- Create: `clients/os/src/apps/materializer/editorOpening.ts`
- Modify: `descriptionHandOff.ts` (its wait for Files entries becomes the exported `useFilesEntries`), `MaterializerApp.tsx` (reserve inside the Make press; hold and release), `CompositionPage.tsx` (`notices` gains `tabRefusal: string`, drawn as an info notice carrying the browser's sentence)
- Test: `clients/os/test/materializer/editorOpening.test.tsx`

**Interfaces:**
- Produces:

```ts
// editorOpening.ts
export interface Reservation { navigate(url: string): void; close(): void }
/** Called synchronously inside the press. Reserves only for a blank or described path with the browser editor; a refused reservation returns its sentence. */
export function reserveEditor(path: MakePath, ports: HandoffPorts, preference: EditorPreference): { reservation: Reservation | null; refusal: string };
// descriptionHandOff.ts: the Files entry of each file, live (the narrow read sub-project 1 built), now shared
export function useFilesEntries(fileIds: readonly string[]): ReadonlyMap<string, { artifactId: string; title: string }>;
// editorOpening.ts
export function useEditorOpening(args: {
  domain: string; compositions: Row[]; entries: ReadonlyMap<string, { artifactId: string; title: string }>;
  outcomes: DescriptionOutcome[]; described: DescribedFile[];
}): { hold(compositionId: string, reservation: Reservation): void; release(compositionId: string): void };
```

The press passes `pathOf(draft, facts)`, decided synchronously inside the click. It says described only where `start`'s own read also will (an unknown file counts as one the Library cannot attach, Task 2), so a tab is never reserved for a file a model writes; a press whose read finds a described file the prediction did not simply opens nothing by itself. `hold` navigates the reservation to `editorArtifactURL(domain, artifactId, title)` once the composition is ready and its entry exists, and, for a described file, once its outcome exists; it navigates once. `release` (Stop, a failure, a cancelled row, a refused start) closes it. A `navigate` that throws on a tab the person closed is ignored.

- [ ] **Step 1: Write the failing tests** (`handoffPorts` injected into `MaterializerApp`; `localStorage` sets the editor preference):
  - `it("a blank file opens in the tab its press reserved, once its Files entry exists")` (`reserve` called during the click; after `ready` and the entry, `navigate` with `https://vscode.memql.example.com/editor/?resource=` + the encoded `memql-file://memql.example.com/artifacts/<artifactId>/Meeting%20notes.md`).
  - `it("a described file opens after its description is handed on")` (no `navigate` before `libraryRequestDocumentRevision` resolves).
  - `it("the reserved tab closes when the file fails, is stopped or is refused")` (three cases, `close` each time, no `navigate`).
  - `it("nothing is reserved for VS Code or a file a model writes")` (preference `vscode`; a `query` ingredient; a described Markdown file with `numbers.csv` picked).
  - `it("a refused tab is said on the page and Open in editor stays")` (`reserve` throws "Allow this app to open a new tab, then open the file again.").
  - `it("a stop after the file was made opens nothing")`.
- [ ] **Step 2: Run, verify failure.** `cd clients/os && npx vitest run test/materializer/editorOpening.test.tsx` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run.** `cd clients/os && npm run typecheck && npx vitest run test/materializer test/onePathCreate.test.ts` → PASS.
- [ ] **Step 5: Commit** with subject `feat(os): a file filed blank opens in the editor by itself (#6034)`.

---

### Task 8: Templates: list, page and Add a template

**Goal:** Templates are a list with pages and an Add a template wizard.

**Files:**
- Rewrite: `clients/os/src/apps/materializer/TemplatesSection.tsx` (the templates list with a view state `list | page | add`; the recipes half stays below it, unchanged, until Task 9)
- Create: `clients/os/src/apps/materializer/TemplatePage.tsx`, `clients/os/src/apps/materializer/AddTemplateWizard.tsx`
- Modify: `acts.ts` (`templateActs`), `actions.ts` (`useTemplateActs().create` returns the id it wrote, under an id held for the wizard), `useCompose.ts` (delete `useTemplateFiles`; the wizard's files are `fileOptions` over `useLibraryFiles(true)`)
- Test: `clients/os/test/materializer/templates.test.tsx`

**Interfaces:**
- Produces: `templateActs(t: TemplateRow): PageActSpec[]` (active `[Archive (text)]`; archived `[Restore (primary)]`); `AddTemplateWizard({ files, accounts, busy, refusal, onAdd(facts: NewTemplateFacts): void, onCancel(): void })`; `TemplatePage({ template, fileTitle, organizationName, acts, onAct, onBack })`.

Add a template: title "Add a template", icon lucide `LayoutTemplate` (confirm `node_modules/lucide-react/dist/esm/icons/layout-template.js` exists; otherwise `PanelsTopLeft`), steps File ("The Library file it is drawn from."; `fileOptions` minus ZIPs, one choice; empty: "Upload the file in Files first."), Kind of file ("What it makes."; `ChoiceStack` of the seven documents, pre-selected from the chosen file's extension), Name ("What to call it."; starts as the file's title without its extension), Description ("What it is for."; optional `Input`), Organization ("Who it is for."; optional, only when `accounts` has an active one). Forward act "Add template"; refusal "The template was not added." with the cluster's sentence. `createComposeTemplate` sends `{ templateId, name, fileId: <the file row's own id as given>, format, accountIds?: [id], description? }`.

- [ ] **Step 1: Write the failing tests:** `it("lists templates with what they ask for, and counts them once read")`; `it("loading is a skeleton, never the empty sentence")`; `it("an empty list says what a template is")`; `it("Add a template asks one thing a step")`; `it("adds a template once however often it is pressed")` (one `createComposeTemplate`); `it("says the cluster's refusal and keeps the answers")`; `it("a template's page archives and restores it")`; `it("draws no disabled act")`. Move the template cases of `app.test.tsx` ("templates and recipes", "organization and reference metadata" for templates) here.
- [ ] **Step 2: Run, verify failure.** `cd clients/os && npx vitest run test/materializer/templates.test.tsx` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run.** `cd clients/os && npm run typecheck && npx vitest run test/materializer` → PASS.
- [ ] **Step 5: Commit** with subject `feat(os): Materializer templates: list, page and Add a template (#6034)`.

---

### Task 9: Recipes is its own tab

**Goal:** Recipes are a tab of their own, with a page that runs one again.

**Files:**
- Create: `clients/os/src/apps/materializer/RecipesSection.tsx`, `clients/os/src/apps/materializer/RecipePage.tsx`
- Modify: `settings.ts` (`{ id: "recipes", name: "Recipes" }` after `templates`), `TemplatesSection.tsx` (the recipes half removed), `acts.ts` (`recipeActs`), `MaterializerApp.tsx` (route `recipes`; Run it again opens the making stage)
- Regenerate: `component/memql/os_navigation.json`, `component/architecture/embedded/platform.graph.json`
- Test: `clients/os/test/materializer/recipes.test.tsx`

**Interfaces:**
- Produces: `recipeActs(r: RecipeRow): PageActSpec[]` (active `[Archive (text), Run it again (primary)]`; archived `[Restore (primary)]`); `RecipePage({ recipe, templateName, filedIn, organizationName, acts, busy, error, onAct, onBack })` (`filedIn` as `CompositionPage`'s).

- [ ] **Step 1: Write the failing tests:** `it("lists recipes with how often they ran")` ("Not run yet"; "Made 2 times, last on " + `formatMoment("2026-06-30T09:00:00Z")`); `it("a run does not ring the list")` (moved from `app.test.tsx`); `it("Run it again makes the file and shows it being made")` (`composeRunRecipe({ recipeId })`, then `navigate("composer")` and "Making the file"); `it("a recipe's page archives and restores it")`; `it("an empty list says how a recipe is made")`.
- [ ] **Step 2: Run, verify failure.** `cd clients/os && npx vitest run test/materializer/recipes.test.tsx` → FAIL.
- [ ] **Step 3: Implement**, then regenerate both files (Global Constraints).
- [ ] **Step 4: Run.** `cd clients/os && npm run typecheck && npx vitest run test/materializer test/ask`; `make platform-graph && MEMQL_DATABASE_DSN='postgres://x:x@127.0.0.1:1/x?sslmode=disable' go test -count=1 -run TestPlatformGraph .` → PASS.
- [ ] **Step 5: Commit** the files above and both generated files with subject `feat(os): Recipes is its own tab with a page (#6034)`.

---

### Task 10: Materialized opens the app, with Make a file on its plus

**Goal:** The window opens on Materialized, whose plus opens Make a file; Make a file is a drill-down; Files opens a composition's page.

**Files:**
- Modify: `clients/os/src/apps/materializer/settings.ts` (sections in this order: `materialized` "Materialized"; `composer` "Make a file" with `parent: "materialized"`; `templates`; `recipes`; `logs`; `settings`. `DEFAULT_MATERIALIZER_SETTINGS.defaultSection = "materialized"`; export `MATERIALIZER_OPENING_SECTIONS = ["materialized", "composer", "templates", "recipes"]`; `sanitizeSettings` keeps only those), `clients/os/src/items/materialize.ts` (`export const MATERIALIZER_MADE_SECTION = "materialized";`), `MaterializedSection.tsx` (Head: `Refine` icon-only with the kit `Select` "Kind of file" / "Any kind" replacing the raw `<select>`, `AddButton` "Make a file"; rows: the kind's glyph, the name, the kind as the second line, `ProvenanceMark`, the state word; `RecordListSkeleton`, `EmptyState` and notices per spec 4), `MaterializerApp.tsx` (the plus: a fresh `draftFromSetup(null, defaultFormat)` and `navigate("composer", { fromContent: true })`; a `{compositionId}` intent opens the page in `materialized` when finished), `clients/os/src/apps/files/BrowseSection.tsx` and `Inspector.tsx` ("Open in Materializer" names `MATERIALIZER_MADE_SECTION`)
- Regenerate: the two generated files
- Test: `clients/os/test/materializer/list.test.tsx`; update `intent.test.tsx`, `clients/os/test/files/materializerPlace.test.tsx` (its fixture manifest's sections)

- [ ] **Step 1: Write the failing tests:**
  - `list.test.tsx`: `it("the window opens on Materialized")`; `it("the plus opens Make a file on From")`; `it("Refine asks the kind with the kit's select")` (a `combobox` named "Kind of file" offering "Any kind" first; no native `select` element under the section); `it("counts after the read settles")`; `it("an empty list invites a file, and says when archived ones are hidden")`; `it("says when nothing matches")`; `it("a failed read is said, never an empty list")` ("This cluster did not return what you made."); and, moved from `app.test.tsx`, the archived preference, the unfiltered read, the arrival cue and the run-state cases.
  - `intent.test.tsx`: the manifest declares `composer` with `parent: "materialized"` and `materialized` first; `MATERIALIZER_MADE_SECTION` is declared; a `{compositionId}` for a ready composition opens its page in `materialized`.
  - `materializerPlace.test.tsx`: "Open in Materializer" calls `openApp("materializer", "materialized", { compositionId })`.
- [ ] **Step 2: Run, verify failure.** `cd clients/os && npx vitest run test/materializer/list.test.tsx test/materializer/intent.test.tsx test/files/materializerPlace.test.tsx` → FAIL.
- [ ] **Step 3: Implement**, then regenerate both files.
- [ ] **Step 4: Run.** `cd clients/os && npm run typecheck && npx vitest run`; `make platform-graph && MEMQL_DATABASE_DSN='postgres://x:x@127.0.0.1:1/x?sslmode=disable' go test -count=1 -run TestPlatformGraph .` → PASS.
- [ ] **Step 5: Commit** with subject `feat(os): Materialized opens the app, with Make a file on its plus (#6034)`.

---

### Task 11: Settings, and a blank file without inference

**Goal:** Settings is one page of four preferences, and the app opens without Inference while the floor says what needs it.

**Files:**
- Create: `clients/os/src/apps/materializer/SettingsSection.tsx` (moved out of `MaterializerApp.tsx`)
- Modify: `settings.ts` (`MATERIALIZER_REQUIRES = ["storage"]`, `MATERIALIZER_WANTS = ["ai"]`), `MaterializerApp.tsx` (`facts.inference` from `gateFor(readiness, ["ai"], [])`: `false` only for `"unconfigured"`, `null` for `"unknown"`, `true` otherwise; `canSetUpInference` (`moduleActFor` answers `open`) and `inferenceElsewhere` (any other answer) from `moduleActFor({ id: "ai", verdict: readiness.of("ai"), ...useReach() })`; `onSetUpInference` opens that act's app and section), `clients/os/test/system/readinessMapping.test.ts`
- Test: `clients/os/test/materializer/settings.test.tsx`

Settings: `Head` "Settings"; `SetupGroup` first (unchanged); a `fieldset` whose legend reads "Preferences" dressed as a Subhead; `Select` "Opening tab" over `MATERIALIZER_OPENING_SECTIONS` by their manifest names; `Select` "Kind of file to start on"; `Switch` "Include archived" (`showArchived`); `Switch` "Offer every record type" (`showUnmarkedConcepts`). No caption, no spending note.

- [ ] **Step 1: Write the failing tests:** `it("is titled Settings and names each preference once")` (no "Materializer settings", no "Spending"); `it("opens on the tabs a person can choose, never Logs or Settings")`; `it("keeps on and off preferences as switches")` (`getByRole("switch", { name: "Include archived" })` toggles the store); `it("opens without Inference and makes a blank file")`; `it("asks for Inference before making a described file, and owners can set it up")` (owner: "Set up Inference" → `openApp("settings", "providers")`; writer: "An owner or developer can set it up."); in `readinessMapping.test.ts`, `requirementsFor(app("materializer"), "composer")` equals `{ requires: ["storage"], wants: ["ai"] }`.
- [ ] **Step 2: Run, verify failure.** `cd clients/os && npx vitest run test/materializer/settings.test.tsx test/system/readinessMapping.test.ts` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run.** `cd clients/os && npm run typecheck && npx vitest run test/materializer test/system test/settings test/kit test/ask` → PASS (the navigation contract does not change: it records no module needs).
- [ ] **Step 5: Commit** with subject `feat(os): Materializer Settings, and a blank file without inference (#6034)`.

---

### Task 12: Files: New Markdown file

**Goal:** Files' Add menu offers New Markdown file, which opens Make a file with Markdown, the current folder and the way back.

**Files:**
- Modify: `clients/os/src/apps/files/BrowseSection.tsx` (`AddMenu` gains `onNewMarkdown?: () => void`, drawn between Upload files and New folder, absent when undefined; the `AddButton` label becomes "Add to this folder"; `BrowseSection` passes `onNewMarkdown` only when `canOpen(registry, MATERIALIZER_APP_ID)`)
- Test: `clients/os/test/files/newMarkdown.test.tsx`

`onNewMarkdown` calls `actions.openApp(MATERIALIZER_APP_ID, MATERIALIZER_COMPOSE_SECTION, composeIntent({ format: "markdown", ...(destinationFolderId ? { folderId: destinationFolderId } : {}), returnTo: { appId: "files", sectionId: "browse" } }))`. The item's icon is lucide `FilePlus`.

- [ ] **Step 1: Write the failing tests** over `test/files/harness.tsx`'s `renderFiles`, spying on `actions.openApp` through a `vi.mock` of `src/chrome/state` whose `useOs` returns the real shell with `actions.openApp` replaced by a spy (the shape `test/accounts/people.test.tsx` uses for `useOsIfPresent`; `BrowseSection` calls `useOs`):
  - `it("opens Make a file with Markdown, this folder and the way back")` (looking at folder `research`: `openApp("materializer", "composer", { compose: { format: "markdown", folderId: "research", returnTo: { appId: "files", sectionId: "browse" } } })`).
  - `it("names no folder at the top of the Library, while searching, in the Bin or in the Materializer place")`.
  - `it("is absent when the Materializer cannot be opened")` (`registryWith(null, "materializer")`).
  - `it("lists Upload files, New Markdown file and New folder under the folder's name")` (menu items in that order; the button's accessible name "Add to this folder").
  - `it("the Materializer reads the setup Files sends and opens on Name")` (`readComposeIntent` of the sent payload, then `openingStep(draftFromSetup(..., "pdf"), facts)` is `"name"` and the kind is Markdown).
- [ ] **Step 2: Run, verify failure.** `cd clients/os && npx vitest run test/files/newMarkdown.test.tsx` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run.** `cd clients/os && npm run typecheck && npx vitest run test/files test/onePathCreate.test.ts` → PASS.
- [ ] **Step 5: Commit** with subject `feat(os): Files: New Markdown file opens the Materializer (#6035)`.

---

### Task 13: Corrections sweep and guards

**Goal:** Nothing of the old composer remains, and guards keep the Materializer inside the interface language.

**Files:**
- Create: `clients/os/test/materializer/corrections.test.ts`
- Modify: `clients/os/src/styles/index.css` (delete the `.os-mz` and `.os-mz-*` rules, about lines 11144-11545 and 15564-15565, and `.os-mz` from the `.os-window-content:has(...)` selector at about line 15557), `Provenance.tsx` (keep `ProvenanceMark`, delete `ProvenanceChain`), `words.ts` (delete `UNOFFERED_FORMATS`, `MATERIALIZED_EMPTY`, `TEMPLATES_EMPTY`, `RECIPES_EMPTY` and anything else with no caller, with their cases in `rows.test.ts`), every comment under `apps/materializer` that still describes columns, the Compose tab or a form
- Delete: `clients/os/test/materializer/app.test.tsx` once its last case has a new home (list each in the commit body)

- [ ] **Step 1: Write the failing guards** (each proven against a planted string, as `onePathCreate.test.ts` proves its patterns): `it("draws no disabled act")` (no `disabled` on a `Button` or an act under `apps/materializer`, except `busy`); `it("has no raw select")`; `it("keeps on and off preferences as switches")` (no `Check` import under `apps/materializer`); `it("parses no id")` (no `idTail`, no `.split(":")` under `apps/materializer`); `it("leaves no old composer class")` (no `os-mz` in `src`); `it("uses every class its stylesheet declares")` (every `.mz-` selector in `materializer.css` appears under `apps/materializer`).
- [ ] **Step 2: Run, verify failure.** `cd clients/os && npx vitest run test/materializer/corrections.test.ts` → FAIL on what remains.
- [ ] **Step 3: Fix** what the guards find and delete the dead code and rules above.
- [ ] **Step 4: Run.** `cd clients/os && npm run typecheck && npx vitest run` (the whole suite, `onePathCreate` included) and the QA bundle check → PASS.
- [ ] **Step 5: Commit** with subject `fix(os): the Materializer meets the interface language everywhere (#6034)`.

---

### Task 14: Documentation

**Goal:** The operator docs and the OS README describe the app as it is.

**Files:**
- Modify: `docs/public/operate/memql-os.md` (rewrite only the "### Materializer" section, current-only: Make a file and its steps; From mixes Library files and records; the folder control; blank files from a name; the request table, updated for the routing of spec 2.10: a name alone is a blank file; Markdown described in words, with nothing picked or only Markdown files (up to 32 KiB each) and pictures (PNG, JPEG, GIF or WebP, up to 8 MiB each), is a blank file whose description the client hands to the revision workflow; Markdown described in words with any other file, a record or a query is written by the composer, which reads them; a template, another kind, an email template or a package is written by the composer; and the OS offers all of them. Replace the paragraph that says a PDF or a spreadsheet is left off a description: only the Library's totals leave a picked file unattached now; the made file's page; the file opening in the editor by itself for blank and described files in the browser editor; Materialized, Templates, Recipes; New Markdown file in Files; that audio and video are not offered; one `materialize` helper and the guard. Leave the app-table row alone), `clients/os/README.md` (replace the Materializer section at about 1579-1850 with a current-only account of this design and keep its "One way to make a file" and "A starting setup" rules, its request table carrying the same routing as `memql-os.md`'s; in the Files section, "The Materializer tab" at about 1326 names the page "Open in Materializer" opens, and the Add menu's three items), `clients/os/qa/list-coverage.md` (the Materializer row: compositions, templates, recipes, the From tray), `clients/os/qa/loading-and-navigation.md` (the Materializer row: pages replace their lists; the wizard's tray groups load as skeletons; the making stage is a running operation)
- Modify: `docs/superpowers/specs/2026-10-12-materializer-app-design.md` (Status: built, with any amendment found while building, the way sub-project 1's record carries its own), `docs/superpowers/specs/2026-10-11-materializer-front-door-design.md` (one line under its amendment 5: which Markdown requests are described now follows `2026-10-12-materializer-app-design.md` D31)

- [ ] **Step 1:** Write the changes in plain, current-only language (no "previously", no "used to", no history of the old composer). `docs/public` never links into `docs/superpowers`.
- [ ] **Step 2: Run.** `git grep -n "Compose another\|OS composer has no folder control\|No app in the shell sends a setup\|picker lists records, not Library files" -- docs/public clients/os/README.md` (expect nothing); `git grep -n "Only Library files travel with a description\|no source that is not a Library file" -- docs/public clients/os/README.md sdk dsl integrations` (expect nothing); `make identity-tailwind && MEMQL_DATABASE_DSN='postgres://x:x@127.0.0.1:1/x?sslmode=disable' go test -count=1 -run 'Docs|RetiredVocabulary|HistoricalDocs' .` → PASS.
- [ ] **Step 3: Commit** the files above with subject `docs: the Materializer app and New Markdown file (#6034, #6035)`.

---

### Task 15: QA scenes

**Goal:** Every Materializer surface and the Files Add menu render by URL, empty and populated.

**Files:**
- Create: `clients/os/qa/materializerScenes.tsx` (exports `MATERIALIZER_VIEWS`, built only from `test/materializer/harness` and `test/files/harness` factories, mounting the real components in `OsProvider` with `OS_REGISTRY`)
- Modify: `clients/os/qa/main.tsx` (one import and `...MATERIALIZER_VIEWS` in `VIEWS`), `clients/os/qa/README.md` (the scene list and what each judges)

**Scenes** (`?view=<name>&mode=light|dark`; `&click=` as the harness supports): `materializer-list`, `materializer-list-empty`, `materializer-list-hidden` (only archived ones), `materializer-make-plus` (From open, a populated tray), `materializer-make-mixed` (two files and a record type picked, counts on the cards), `materializer-make-from-files` (a Files setup: Name open), `materializer-make-ready` (a name and a description: Ready), `materializer-make-blank-pdf` (the blank blocker), `materializer-make-inference` (the Inference blocker, owner), `materializer-make-refused` (the folder refusal, "Unavailable folder"), `materializer-making` (busy, 0:12), `materializer-file-ready`, `materializer-file-trip` (Back to Files), `materializer-file-described` (not sent and not attached notices), `materializer-file-failed`, `materializer-file-package`, `materializer-file-archived`, `materializer-templates`, `materializer-templates-empty`, `materializer-template-add`, `materializer-template`, `materializer-recipes`, `materializer-recipes-empty`, `materializer-recipe`, `materializer-settings`, `files-add-menu` (`files` with `&click=Add to this folder`).

- [ ] **Step 1:** Add the scenes and the README lines. Nothing is faked beyond the two harnesses; the wizard scenes pass their drafts as props.
- [ ] **Step 2: Run.** The QA bundle check, then `cd clients/os && npm run typecheck && npx vitest run test/materializer test/files` → PASS.
- [ ] **Step 3: Commit** `clients/os/qa/materializerScenes.tsx clients/os/qa/main.tsx clients/os/qa/README.md` with subject `test(os): QA scenes for every Materializer surface and the Files Add menu (#6034, #6035)`.

---

### Task 16: Screenshots and critique (scratchpad only)

**Goal:** Every scene is judged at real size in both modes, and its defects are fixed.

**Files:**
- Create (scratchpad, never committed): `$SCRATCH/mz-shots/capture.sh` (function-based bash: `start_server`, `wait_for_server`, `capture_scene`, `capture_all`, `stop_server`, `write_index`, `main`), the PNGs, `$SCRATCH/mz-shots/index.md`

- [ ] **Step 1: Capture.** `capture.sh` starts `npx vite --config qa/vite.config.ts --port 5199 --strictPort` from `clients/os` in the background, waits for `http://localhost:5199/`, then for every Task 15 scene, both modes and both widths runs:

```bash
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" --headless=new --disable-gpu --no-sandbox \
  --hide-scrollbars --window-size=1400,900 --virtual-time-budget=12000 \
  --screenshot="$SCRATCH/mz-shots/<scene>-<mode>-1400.png" "http://localhost:5199/?view=<scene>&mode=<mode>"
# and again with --window-size=520,900 into <scene>-<mode>-520.png
```

It writes `index.md` (one line per scene, its four files) and stops the server. `git status` stays clean.
- [ ] **Step 2: Critique** each scene: one Head per scroller; acts only in the bar, at most three, one button; no disabled control; skeletons, not captions; the rail readable as a recipe at 520; the cards wrap without a horizontal scroll; focus visible; light and dark legible; and one "remove an accessory" pass per surface, recording what was removed or that nothing could be. Record findings beside each scene in `index.md`.
- [ ] **Step 3: Fix** each defect in its own commit, test-first where it is behaviour (`fix(os): <what> (#6034)`), and re-capture the affected scenes.
- [ ] **Step 4: Report** the `index.md` path to the controller.

---

### Task 17: Live check on the local cluster (controller, after the owner says go)

**Goal:** The owner's local cluster shows the whole trip working, used as a person uses it.

The owner demos from the local cluster. The controller asks before any of this, and runs it; an implementer does not.

- [ ] **Step 1: Ask the owner** for a go-ahead to rebuild the local cluster's images.
- [ ] **Step 2: Deploy.** `make dev` (the OS client ships in the edge image; Task 1a's routing and the regenerated navigation catalog are in the engine images, the catalog embedded through `component/memql/work_navigation.go`). Wait for `make status`.
- [ ] **Step 3: Check like a person**, in Chrome at `https://os.memql.localhost`, recording what was seen:
  - Files, a folder, Add to this folder, New Markdown file: the Materializer opens on Name with Markdown and that folder; type a name; Make the file; the browser editor opens the empty document; the OS page offers Back to Files; Back to Files shows the file in that folder.
  - The same with a description and a Markdown reference picked in From: the editor opens on its draft being prepared; approve the draft in the editor.
  - A Markdown file described in words with a CSV picked beside a Markdown file: the floor says "A model writes it."; the making stage, then the page; Open in editor shows text drawn from the CSV; the editor shows no revision waiting.
  - From the Materializer's plus: a PDF from two Library files and a record type with a description; the making stage, then the page; Open in editor.
  - Folder refusal: archive the folder between opening and pressing; "That folder isn't available. Choose another folder."; choose another folder; the file is made there.
  - Stop during a model-written file; the page reads Stopped.
  - With the editor preference set to VS Code: nothing opens by itself; Open in editor hands off.
  - Templates: Add a template from a `.docx`; archive and restore it. Recipes: save a recipe from a page, Run it again.
- [ ] **Step 4: Report** to the owner what worked and what did not, with screenshots from the session. Defects found here go back through Task 16's fix loop.
- [ ] **Step 5: At the epic's merge**, `git rm docs/superpowers/plans/2026-10-12-materializer-app.md` (a spent plan is deleted in the epic's own merge, `docs/CLAUDE.md`) and commit it with subject `docs: retire the Materializer app plan (#6034, #6035)`.
