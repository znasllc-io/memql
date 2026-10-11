# Materializer front door Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the Materializer the only way a new file is created in MemQL, for the OS and both editor extensions, without weakening the Markdown writing flow.

**Architecture:** `composeMaterialize` gains blank requests, a `requestId` for idempotency, and a final `composeRequestRevision` step that hands a described Markdown file to the existing Library revision workflow through engine-routed builtins. A transport-neutral SDK module (`startMaterialize` / `followComposition` / `materialize`) is the one client entry point; VS Code and the OS Materializer app move onto it, and a guard test forbids other creation paths.

**Tech Stack:** Go (integrations/compose, component/compose), MemQL DSL (dsl/compose), TypeScript SDK (`sdk/ts`, node:test), VS Code productivity extension (`editors/productivity`, node:test with the vscode fake), React OS client (`clients/os`, vitest).

**Spec:** `docs/superpowers/specs/2026-10-11-materializer-front-door-design.md` (read it first, including "Amendments from planning").

**Tracking:** epic #6023 (program #6022); task N is issue #6023+N (Task 1 = #6024 ... Task 8 = #6031). Reference the task's issue number in each commit.

## Global Constraints

- Pre-release: no backwards-compat shims, no fallback code paths. Fix every caller in the same change.
- Stage files by explicit path (`git add <file>`); never `git add -A` / `git add .`.
- Clients use bare ids only; never compose, parse or compare canonical ids client-side.
- Blank formats: `markdown`, `txt`, `html`, `csv`, `json`. Blank `docx`/`pdf` refused with exactly: `A blank file can be Markdown, text, HTML, CSV or JSON.`
- Blank Markdown bytes are exactly `"\n"` (no front matter) so the editor's empty-document page appears.
- Cross-node: `composeRequestRevision` calls `libraryAddDocumentComment` and `libraryRequestDocumentRevision` through the engine as named builtins, never by importing `integrations/library`.
- A good output file is never deleted because a later step failed.
- User-facing copy: sentence case, no emojis, says what happened and what to do (`clients/os/DESIGN.md`, `docs/internal/design/2026-09-28-vscode-extension-ux.md`).
- After DSL changes run `make sdk-gen` and commit the regenerated `sdk/go/client` + `sdk/ts/src/client` files; `make sdk-gen-check` must pass.
- New worktrees: run `make identity-tailwind` before root-package tests.
- Disk is tight on the owner's Mac: do not `npm ci` in more than one worktree at a time; remove `node_modules` in scratch worktrees when done.

## Review Focus

1. **Double submit from VS Code** (person presses Enter twice on the name box): exactly one composition and one file. Test in Task 5.
2. **Described Markdown whose artifact index row is slow** (the `indexFileOnCreate` automation has not run yet): the revision step waits a bounded time, then records `revisionFailure` instead of hanging or failing the composition. Test in Task 2.
3. **Name already ends in `.md` / contains a slash**: the produced filename is `<stem>.md`, never `x.md.md`, and a slash is refused before any composition is written. Test in Task 1.
4. **Cancelling while the Files entry is still pending**: `composeCancel` is called once and the helper stops polling; the filed blank file stays. Test in Task 4.
5. **OS intent with a stale or foreign folder id**: the Compose section pre-fills format and sources and drops a folder the person cannot read, with the server refusing rather than filing elsewhere. Test in Task 6.

---

### Task 1: Blank requests and `requestId` idempotency

**Files:**
- Modify: `dsl/compose/builtins.memql:28-43` (add `requestId` to `composeMaterialize`; add `composeBlankDraft` step builtin after `composeRequireDraft`)
- Modify: `dsl/compose/automations.memql:14-39` (blank branch)
- Modify: `integrations/compose/materialize.go` (`materializeArgs`, `parseMaterializeArgs`, `materialize`, step map in `executePipeline`, `composeWorkflowFacts`)
- Modify: `integrations/compose/integration.go` (register `composeBlankDraft` where the other step executors are registered; grep `composeRequireDraft` to find the spot)
- Modify: `component/compose/write.go` (blank rendering helper)
- Test: `integrations/compose/materialize_test.go`, `integrations/compose/materialize_db_test.go`, `component/compose/write_test.go` (create if absent)
- Regenerate: `sdk/go/client/*`, `sdk/ts/src/client/generated_builtins.ts`

**Interfaces:**
- Produces: `materializeArgs.RequestId string`; `func (a materializeArgs) blank() bool`; `func pure.RenderBlank(f pure.Format, title string) (pure.Result, error)`, `var pure.ErrBlankFormat error`; workflow fact `isBlank bool`; SDK arg `ComposeMaterializeArgs.requestId?: string`.

- [ ] **Step 1: Write failing unit tests** in `integrations/compose/materialize_test.go`:

```go
func blankArgs(format pure.Format) materializeArgs {
	return materializeArgs{Name: "Notes", Format: format}
}

func TestMaterializeBlankMarkdownFilesAnEmptyDocumentWithoutAModelCall(t *testing.T) {
	i, e, u := materializeFixture(t)
	out, err := i.materialize(nestedMaterializeContext(), "u-alice", "", blankArgs(pure.FormatMarkdown))
	if err != nil {
		t.Fatal(err)
	}
	if out["status"] != "ready" {
		t.Fatalf("blank composition did not complete: %v", out)
	}
	if string(u.bytes) != "\n" {
		t.Fatalf("blank markdown must be a single newline so the editor shows its empty page, got %q", u.bytes)
	}
	for _, f := range e.files {
		if f["name"] != "Notes.md" {
			t.Fatalf("blank markdown filename = %v, want Notes.md", f["name"])
		}
	}
}

func TestMaterializeBlankFormatsRenderTheirEmptyShape(t *testing.T) {
	cases := map[pure.Format]string{
		pure.FormatText:     "",
		pure.FormatCSV:      "",
		pure.FormatJSON:     "{}\n",
		pure.FormatHTML:     "<!doctype html>\n<html><head><meta charset=\"utf-8\"><title>Notes</title></head><body></body></html>\n",
		pure.FormatMarkdown: "\n",
	}
	for format, want := range cases {
		i, _, u := materializeFixture(t)
		if _, err := i.materialize(nestedMaterializeContext(), "u-alice", "", blankArgs(format)); err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if string(u.bytes) != want {
			t.Fatalf("%s blank bytes = %q, want %q", format, u.bytes, want)
		}
	}
}

func TestMaterializeRefusesBlankDocxAndPdfBeforeWritingAnything(t *testing.T) {
	for _, format := range []pure.Format{pure.FormatDOCX, pure.FormatPDF} {
		i, e, _ := materializeFixture(t)
		_, err := i.materialize(nestedMaterializeContext(), "u-alice", "", blankArgs(format))
		if err == nil || !strings.Contains(err.Error(), "A blank file can be Markdown, text, HTML, CSV or JSON.") {
			t.Fatalf("%s: want blank refusal, got %v", format, err)
		}
		if len(e.compositions) != 0 || len(e.inputs) != 0 {
			t.Fatalf("%s: refusal wrote rows: %v %v", format, e.compositions, e.inputs)
		}
	}
}

func TestParseMaterializeArgsStripsAMatchingExtensionAndRefusesPaths(t *testing.T) {
	a, err := parseMaterializeArgs(map[string]any{"name": "Plan.md", "format": "markdown"})
	if err != nil || a.Name != "Plan" {
		t.Fatalf("name = %q err = %v, want Plan", a.Name, err)
	}
	if _, err := parseMaterializeArgs(map[string]any{"name": "a/b", "format": "markdown"}); err == nil {
		t.Fatal("a name with a slash must be refused")
	}
}

func TestMaterializeSameRequestIdReturnsTheSameComposition(t *testing.T) {
	i, e, u := materializeFixture(t)
	opener := &recordingGoalOpener{}
	i.SetGoalOpener(opener)
	a := blankArgs(pure.FormatMarkdown)
	a.RequestId = "req-1"
	first, err := i.materialize(materializeContext(), "u-alice", "", a)
	if err != nil {
		t.Fatal(err)
	}
	again, err := i.materialize(materializeContext(), "u-alice", "", a)
	if err != nil {
		t.Fatal(err)
	}
	if first["compositionId"] != again["compositionId"] || len(e.compositions) != 1 || opener.calls != 1 {
		t.Fatalf("repeat started a second composition: first=%v again=%v rows=%d opens=%d uploads=%d", first, again, len(e.compositions), opener.calls, u.calls)
	}
}
```

`recordingGoalOpener` may already exist in the test file (grep `OpenDirectGoal` in `materialize_test.go`); if not, add:

```go
type recordingGoalOpener struct{ calls int }

func (o *recordingGoalOpener) OpenDirectGoal(ctx context.Context, g work.DirectGoal) (string, string, error) {
	o.calls++
	if g.BeforeRun != nil {
		if err := g.BeforeRun(ctx, "goal-1", "run-1"); err != nil {
			return "", "", err
		}
	}
	return "goal-1", "run-1", nil
}
```

(match the opener interface's exact method set by reading `goalOpenerRef()` in `integration.go`).

- [ ] **Step 2: Run, verify failure**

Run: `go test ./integrations/compose/ -run 'Blank|RequestId|StripsAMatching' -count=1`
Expected: FAIL (`RequestId` undefined; blank refused with "nothing to render").

- [ ] **Step 3: Implement**

`component/compose/write.go`:

```go
// RenderBlank writes the empty document for a format. No provenance is
// embedded: a blank file's history lives on its composition row, and a
// Markdown file with front matter would not read as empty in the editor.
func RenderBlank(f Format, title string) (Result, error) {
	var out string
	switch f {
	case FormatMarkdown:
		out = "\n"
	case FormatText, FormatCSV:
		out = ""
	case FormatJSON:
		out = "{}\n"
	case FormatHTML:
		out = "<!doctype html>\n<html><head><meta charset=\"utf-8\"><title>" + html.EscapeString(title) + "</title></head><body></body></html>\n"
	default:
		return Result{}, ErrBlankFormat
	}
	return Result{Bytes: []byte(out), Embedded: false, Note: ""}, nil
}

// ErrBlankFormat is the person-facing refusal for a blank docx or pdf.
var ErrBlankFormat = errors.New("A blank file can be Markdown, text, HTML, CSV or JSON.")
```

`integrations/compose/materialize.go`:
- add `RequestId string` to `materializeArgs`; parse `strings.TrimSpace(stringOf(args["requestId"]))`.
- in `parseMaterializeArgs`, after parsing `format`: refuse `strings.ContainsAny(out.Name, "/\\")` with `errors.New("compose: enter a file name without a folder path")`; strip a trailing `"."+string(format)` (case-insensitive; for markdown also `.markdown`) from `out.Name`.
- add

```go
// blank is a request with nothing to compose from: no words, no draft, no
// sections, no sources, no template, recipe or special output.
func (a materializeArgs) blank() bool {
	return a.Statement == "" && strings.TrimSpace(a.Draft) == "" && len(a.SectionKeys) == 0 &&
		len(a.Sources) == 0 && a.TemplateId == "" && a.RecipeId == "" && a.OutputKind == "" && a.DeployableKind == ""
}
```

- at the top of `materialize`, after the email check: `if a.blank() && a.Format != pure.FormatMarkdown && a.Format != pure.FormatText && a.Format != pure.FormatHTML && a.Format != pure.FormatCSV && a.Format != pure.FormatJSON { return nil, pure.ErrBlankFormat }`.
- requestId: when `!nested && a.RequestId != ""`, set `compositionId = stableMaterializeID("request", memql.BareShortId(userId), a.RequestId)`, look it up with `i.store().compositionById(ctx, compositionId)`; if a row exists return `map[string]any{"compositionId": compositionId, "goalId": row["goalId"], "runId": row["runId"], "format": row["format"], "status": row["status"]}` without writing.
- `composeWorkflowFacts`: add `"isBlank": a.blank() && request.Recipe == nil`.
- step `composeBlankDraft`: sets a local `blank = true`. In `composeRenderOutput`, when `blank`, replace `pure.RenderRecipeBytes(recipe)` with `pure.RenderBlank(a.Format, a.Name)` (still save the recipe snapshot so recovery works).
- `composeRecoverOutput`'s `needsSource` must be false for blank requests (`needsSource = !a.blank() && (...existing...)`).

`dsl/compose/automations.memql` — replace the draft `if` chain head:

```memql
  input := builtin composeWorkflowFacts()
  if input.isBlank {
    builtin composeBlankDraft()
  } else if input.savedRecipe {
```

`dsl/compose/builtins.memql`: add to `composeMaterialize`
`requestId      string    @description("Caller-chosen id for this request. Repeating it returns the same composition instead of starting another, so a double click makes one file.")`
and the step builtin:

```memql
/// Bounded operation in the current owner and composition run.
@executor("integration.compose.composeBlankDraft")
builtin composeBlankDraft { }
```

- [ ] **Step 4: Run tests**

Run: `go test ./integrations/compose/ ./component/compose/ -count=1`
Expected: PASS (all existing tests too).

- [ ] **Step 5: DB-gated test** (proves the engine accepts the writes; runs on CI Postgres). Add to `materialize_db_test.go`, following `TestMaterializeDB_SharedCompositionKeepsSourcesPrivate`'s setup:

```go
func TestMaterializeDB_BlankMarkdownIsReadyWithOneFile(t *testing.T) {
	e := materializeDBEngine(t)
	i := New(e, e.Logger)
	i.SetGoalOpener(work.New(e, e.Logger))
	u := &materializeUploader{}
	i.SetUploader(u, "files")
	owner := fmt.Sprintf("compose-blank-%d", time.Now().UnixNano())
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
	args := materializeArgs{Name: "Blank", Format: pure.FormatMarkdown, RequestId: "r-" + owner}
	out, err := i.materialize(ctx, owner, "", args)
	if err != nil {
		t.Fatal(err)
	}
	row := waitComposition(t, i, ctx, stringOf(out["compositionId"]), "ready")
	if stringOf(row["outputFileId"]) == "" || string(u.bytes) != "\n" {
		t.Fatalf("blank db run: row=%v bytes=%q", row, u.bytes)
	}
}
```

If no `waitComposition` helper exists, add one that polls `i.store().compositionById` every 200ms for up to 20s and fails with the last row.

Run: `go test ./integrations/compose/ -run DB_Blank -count=1` (skips locally without Postgres; CI runs it).

- [ ] **Step 6: Regenerate SDK and commit**

```bash
make sdk-gen && make sdk-gen-check
git add dsl/compose/builtins.memql dsl/compose/automations.memql integrations/compose/materialize.go integrations/compose/integration.go integrations/compose/materialize_test.go integrations/compose/materialize_db_test.go component/compose/write.go component/compose/write_test.go sdk/go/client sdk/ts/src/client
git commit -m "feat(compose): blank files and requestId idempotency (#6024)"
```

---

### Task 2: Hand described Markdown to the Library revision workflow

**Files:**
- Modify: `dsl/compose/concepts.memql` (`composition`: add `revisionId`, `revisionFailure`)
- Modify: `dsl/compose/mutations.memql` (`updateCompositionState` accepts the two fields; grep the mutation)
- Modify: `dsl/compose/builtins.memql` (step builtin `composeRequestRevision`)
- Modify: `dsl/compose/automations.memql` (final step)
- Modify: `integrations/compose/materialize.go` (step + facts), `integrations/compose/integration.go` (register executor), `integrations/compose/store.go` (two engine calls + artifact lookup)
- Test: `integrations/compose/materialize_test.go`, `integrations/compose/materialize_db_test.go`

**Interfaces:**
- Consumes: Task 1 `blank()`, `RenderBlank`.
- Produces: `func (a materializeArgs) describedMarkdown() bool`; composition fields `revisionId string`, `revisionFailure string`; workflow fact `handsToRevision bool`.

- [ ] **Step 1: Failing tests** (extend the fake `materializeEngine.Execute` switch with `libraryArtifactForFile`, `libraryAddDocumentComment`, `libraryDocumentReview`, `libraryRequestDocumentRevision`, recording args, plus a knob `artifactAfter int` = number of `libraryArtifactForFile` calls that return no row before one appears, and `failRevision bool`):

```go
func describedArgs() materializeArgs {
	return materializeArgs{Name: "Launch brief", Format: pure.FormatMarkdown, Statement: "A one-page launch brief for the autumn release"}
}

func TestDescribedMarkdownFilesBlankThenOpensARevision(t *testing.T) {
	i, e, u := materializeFixture(t)
	out, err := i.materialize(nestedMaterializeContext(), "u-alice", "", describedArgs())
	if err != nil {
		t.Fatal(err)
	}
	if string(u.bytes) != "\n" {
		t.Fatalf("described markdown must start blank, got %q", u.bytes)
	}
	row := e.compositions[stringOf(out["compositionId"])]
	if row["status"] != "ready" || stringOf(row["revisionId"]) == "" {
		t.Fatalf("revision not recorded: %v", row)
	}
	comment := e.lastArgs["libraryAddDocumentComment"]
	if comment["body"] != describedArgs().Statement || comment["expectedRevision"] != "file:1" {
		t.Fatalf("comment = %v", comment)
	}
	if e.callIndex("composeGenerateDraft") >= 0 {
		t.Fatal("described markdown must not call the single-shot composer")
	}
}

func TestDescribedMarkdownWaitsForTheArtifactIndexRow(t *testing.T) {
	i, e, _ := materializeFixture(t)
	e.artifactAfter = 3
	i.SetArtifactPoll(5, time.Millisecond)
	out, err := i.materialize(nestedMaterializeContext(), "u-alice", "", describedArgs())
	if err != nil {
		t.Fatal(err)
	}
	if stringOf(e.compositions[stringOf(out["compositionId"])]["revisionId"]) == "" {
		t.Fatal("revision should open once the index row appears")
	}
}

func TestDescribedMarkdownKeepsTheFileWhenTheRevisionCannotOpen(t *testing.T) {
	i, e, _ := materializeFixture(t)
	e.artifactAfter = 100
	i.SetArtifactPoll(3, time.Millisecond)
	out, err := i.materialize(nestedMaterializeContext(), "u-alice", "", describedArgs())
	if err != nil {
		t.Fatal(err)
	}
	row := e.compositions[stringOf(out["compositionId"])]
	if row["status"] != "ready" || stringOf(row["outputFileId"]) == "" || stringOf(row["revisionFailure"]) == "" {
		t.Fatalf("file must stay with revisionFailure recorded: %v", row)
	}
}

func TestDescribedMarkdownWithATemplateStillUsesTheComposer(t *testing.T) {
	i, e, _ := materializeFixture(t)
	a := describedArgs()
	a.TemplateId = "tpl"
	// templateById returns a readable template in the fake; composer fake returns a draft.
	i.SetComposer(fakeComposer{body: "Drafted"})
	if _, err := i.materialize(nestedMaterializeContext(), "u-alice", "", a); err != nil {
		t.Fatal(err)
	}
	if e.callIndex("libraryRequestDocumentRevision") >= 0 {
		t.Fatal("a template request must not be handed to the revision workflow")
	}
}
```

Add `lastArgs map[string]map[string]any` and `callIndex(name string) int` to the fake; add `templateById` returning `{name:"tpl"}` and a `fakeComposer` if absent (grep existing composer fakes first and reuse).

Also add one test that a `library_file` source becomes a comment attachment: reuse whatever fake rows `TestMaterializeDB_SharedCompositionKeepsSourcesPrivate` and the existing source tests use to resolve a library file (grep `library_file` in `materialize_test.go` / `source_content_test.go`), and assert `lastArgs["libraryAddDocumentComment"]["attachments"]` has one entry.

- [ ] **Step 2: Run, verify failure**

Run: `go test ./integrations/compose/ -run DescribedMarkdown -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
// describedMarkdown is written by the Library revision workflow rather than
// the single-shot composer: research, images and an approved proposal.
func (a materializeArgs) describedMarkdown() bool {
	return a.Format == pure.FormatMarkdown && a.Statement != "" && strings.TrimSpace(a.Draft) == "" &&
		len(a.SectionKeys) == 0 && a.TemplateId == "" && a.RecipeId == "" && a.OutputKind == "" && a.DeployableKind == ""
}
```

- `composeWorkflowFacts`: `"isBlank": (a.blank() || a.describedMarkdown()) && request.Recipe == nil`, `"handsToRevision": a.describedMarkdown()`.
- New operation `composeRequestRevision` (runs after `composeCompleteOutput`):
  1. poll `st.libraryArtifactForFile(ctx, fileId)` up to `i.artifactPollAttempts` (default 30) every `i.artifactPollInterval` (default 1s) until a row with an `id` appears; add `SetArtifactPoll(attempts int, interval time.Duration)` mirroring `integrations/library/analysis.go:174`.
  2. `st.call(ctx, "libraryAddDocumentComment", {artifactId, expectedVersion: 1, expectedRevision: "file:1", anchor: {kind:"document", quote:"Entire document"}, body: a.Statement, requestId: stableMaterializeID("comment", compositionId), attachments: <one {artifactId, version, revision} per library_file source whose artifact resolves>})` through the engine (the same `i.store()` execute path used for `createLibraryFile`, under the run's actor, NOT `writeInternal`).
  3. read `libraryDocumentReview({artifactId})` and take the comment id whose requestId matches; then `libraryRequestDocumentRevision({artifactId, expectedVersion: 1, expectedRevision: "file:1", commentIds: [id], instruction: "", requestId: stableMaterializeID("revision", compositionId)})`.
  4. on success `updateCompositionState({compositionId, revisionId: <requestId>})`; on any error `updateCompositionState({compositionId, revisionFailure: "The description could not be sent for writing: " + err.Error()})` and return `nil, nil` (the run succeeds; the file stays).
  The stable request ids make a retried step return the original receipts (both builtins dedupe on `requestId`).
- `dsl/compose/automations.memql`, after `builtin composeCompleteOutput()`:

```memql
  if input.handsToRevision { builtin composeRequestRevision() }
```

- concepts: add to `composition`

```memql
  revisionId      string  @description("Library revision request opened for a described Markdown file. The draft arrives as a proposal the person approves in the editor.")
  revisionFailure string  @description("Why the description could not be sent for writing, when it could not. The file still exists; the person can describe it again in the editor.")
```

and add both as optional args on `updateCompositionState`.

- [ ] **Step 4: Run tests**

Run: `go test ./integrations/compose/ ./dsl/ -count=1`
Expected: PASS.

- [ ] **Step 5: DB-gated test** `TestMaterializeDB_DescribedMarkdownOpensARevision` in `materialize_db_test.go`: same setup as Task 1 Step 5 but also register the Library integration on the engine the way `app/integrations_library.go` does, run `describedArgs()` (no sources), wait for `ready`, then poll `libraryDocumentRevisionStatus({requestId: row.revisionId})` and assert a row returns. If wiring the Library integration in a DB test is not possible without the app package, assert instead that `revisionFailure` is empty OR names the missing executor, and add the full assertion to Task 3's cluster test; note which in the commit body.

- [ ] **Step 6: Regenerate + commit**

```bash
make sdk-gen && make sdk-gen-check
git add dsl/compose integrations/compose sdk/go/client sdk/ts/src/client
git commit -m "feat(compose): hand described Markdown to the Library revision workflow (#6025)"
```

---

### Task 3: Cross-node coverage

**Files:**
- Create: `test/clustere2e/materializer_revision_hop_test.go`
- Read first: `test/clustere2e/automation_run_routing_test.go` (how a two-node harness is built and how a builtin is pinned to one node), `test/clustere2e/workbench_remote_hop_test.go`.

**Interfaces:**
- Consumes: Task 2's `revisionId` field and step.

- [ ] **Step 1: Write the test.** Build the two-node harness used by `automation_run_routing_test.go`. Register the compose integration only on node A and the library integration only on node B. As a writer, call `composeMaterialize({name:"Hop", format:"markdown", statement:"Two paragraphs about the hop", requestId:"hop-1"})` on node A. Poll `compositionById` until `ready`, then assert `revisionId != ""` and `revisionFailure == ""`, and that `libraryDocumentRevisionStatus({requestId: revisionId})` read on node A returns a row whose `proposal.artifactId` equals the artifact for `outputFileId`.
- [ ] **Step 2: Prove it bites.** Temporarily change `composeRequestRevision` to call the library Go handler directly (or to use a local-only executor) and confirm the test FAILS; revert.
- [ ] **Step 3: Run**

Run: `go test ./test/clustere2e/ -run MaterializerRevisionHop -count=1`
Expected: PASS (skips without Postgres locally; CI runs it).

- [ ] **Step 4: Commit**

```bash
git add test/clustere2e/materializer_revision_hop_test.go
git commit -m "test(clustere2e): described Markdown revision crosses nodes (#6026)"
```

---

### Task 4: SDK `materialize` module

**Files:**
- Create: `sdk/ts/src/client/materialize.ts`
- Modify: `sdk/ts/src/client/index.ts` (export it)
- Test: `sdk/ts/test/materialize.test.ts`

**Interfaces:**
- Consumes: generated `buildComposeMaterialize`, `buildCompositionById`, `buildComposeCancel`, `buildLibraryArtifactForFile` (with Task 1's `requestId`).
- Produces (exact):

```ts
export type MaterializeExecute = (name: string, call: string) => Promise<Record<string, unknown>[]>;
export type MaterializeFormat = "markdown" | "html" | "txt" | "csv" | "json" | "docx" | "pdf";
export interface MaterializeSource { kind: "concept_row" | "library_file" | "query"; ref: string; label: string; content?: boolean; includeImages?: boolean }
export interface MaterializeRequest { name: string; format: MaterializeFormat; requestId: string; statement?: string; sources?: MaterializeSource[]; templateId?: string; folderId?: string; outputKind?: "email_template"; accountIds?: string[]; recipeId?: string; deployableKind?: string }
export interface MaterializePorts { execute: MaterializeExecute; signal?: AbortSignal; onStatus?: (status: string) => void; intervalMs?: number }
export interface MaterializeResult { compositionId: string; fileId: string; artifactId: string; name: string; revisionId?: string; revisionFailure?: string }
export class MaterializeCancelled extends Error {}
export class MaterializeFailed extends Error {}
export function startMaterialize(execute: MaterializeExecute, request: MaterializeRequest): Promise<string>;
export function followComposition(ports: MaterializePorts, compositionId: string): Promise<MaterializeResult>;
export function materialize(ports: MaterializePorts, request: MaterializeRequest): Promise<MaterializeResult>;
```

- [ ] **Step 1: Failing tests** (`sdk/ts/test/materialize.test.ts`):

```ts
import test from "node:test";
import assert from "node:assert/strict";
import { materialize, followComposition, MaterializeCancelled, MaterializeFailed } from "../src/client/materialize.js";

function fake(script: Record<string, Array<Record<string, unknown>[]>>) {
  const calls: { name: string; call: string }[] = [];
  const execute = async (name: string, call: string) => {
    calls.push({ name, call });
    const queue = script[name] ?? [[]];
    return queue.length > 1 ? queue.shift()! : queue[0];
  };
  return { calls, execute };
}

test("materialize starts once, follows to ready, and resolves the artifact", async () => {
  const f = fake({
    composeMaterialize: [[{ compositionId: "c1" }]],
    compositionById: [[{ status: "composing" }], [{ status: "ready", outputFileId: "f1", name: "Notes" }]],
    libraryArtifactForFile: [[], [{ id: "a1", title: "Notes.md" }]],
  });
  const seen: string[] = [];
  const out = await materialize({ execute: f.execute, onStatus: s => seen.push(s), intervalMs: 1 }, { name: "Notes", format: "markdown", requestId: "r1" });
  assert.deepEqual(out, { compositionId: "c1", fileId: "f1", artifactId: "a1", name: "Notes.md" });
  assert.equal(f.calls.filter(c => c.name === "composeMaterialize").length, 1);
  assert.match(f.calls[0].call, /requestId:\s*"r1"/);
  assert.ok(seen.includes("composing") && seen.includes("Saved; waiting for its Files entry"));
});

test("a failed composition throws its reason", async () => {
  const f = fake({ compositionById: [[{ status: "failed", failureReason: "No access to notes.md" }]] });
  await assert.rejects(followComposition({ execute: f.execute, intervalMs: 1 }, "c1"), (e: Error) => e instanceof MaterializeFailed && e.message === "No access to notes.md");
});

test("aborting cancels the composition once and stops polling", async () => {
  const controller = new AbortController();
  const f = fake({ compositionById: [[{ status: "ready", outputFileId: "f1" }]], libraryArtifactForFile: [[]] });
  const run = followComposition({ execute: f.execute, signal: controller.signal, intervalMs: 5 }, "c1");
  setTimeout(() => controller.abort(), 12);
  await assert.rejects(run, (e: Error) => e instanceof MaterializeCancelled);
  const before = f.calls.length;
  await new Promise(r => setTimeout(r, 20));
  assert.equal(f.calls.filter(c => c.name === "composeCancel").length, 1);
  assert.equal(f.calls.length, before, "polling continued after abort");
});

test("revision fields pass through", async () => {
  const f = fake({
    compositionById: [[{ status: "ready", outputFileId: "f1", revisionId: "rev", revisionFailure: "" }]],
    libraryArtifactForFile: [[{ id: "a1", title: "Brief.md" }]],
  });
  const out = await followComposition({ execute: f.execute, intervalMs: 1 }, "c1");
  assert.equal(out.revisionId, "rev");
  assert.equal(out.revisionFailure, undefined);
});

test("no compositionId in the reply is an explicit error", async () => {
  const f = fake({ composeMaterialize: [[{}]] });
  await assert.rejects(materialize({ execute: f.execute, intervalMs: 1 }, { name: "x", format: "txt", requestId: "r" }), /did not confirm a composition/);
});
```

- [ ] **Step 2: Run, verify failure**

Run: `cd sdk/ts && npm test` (after `npm ci` once)
Expected: FAIL (module missing).

- [ ] **Step 3: Implement** `sdk/ts/src/client/materialize.ts`:

```ts
// The one way a client creates a file (design: docs/superpowers/specs/2026-10-11-materializer-front-door-design.md).
// Transport-neutral: the OS passes query.executeNamed, editors pass EditorConnectionAPI.execute.
import { buildComposeMaterialize, buildComposeCancel } from "./generated_builtins.js";
import { buildCompositionById, buildLibraryArtifactForFile } from "./generated_queries.js";

export type MaterializeExecute = (name: string, call: string) => Promise<Record<string, unknown>[]>;
export type MaterializeFormat = "markdown" | "html" | "txt" | "csv" | "json" | "docx" | "pdf";
export interface MaterializeSource { kind: "concept_row" | "library_file" | "query"; ref: string; label: string; content?: boolean; includeImages?: boolean }
export interface MaterializeRequest { name: string; format: MaterializeFormat; requestId: string; statement?: string; sources?: MaterializeSource[]; templateId?: string; folderId?: string; outputKind?: "email_template"; accountIds?: string[]; recipeId?: string; deployableKind?: string }
export interface MaterializePorts { execute: MaterializeExecute; signal?: AbortSignal; onStatus?: (status: string) => void; intervalMs?: number }
export interface MaterializeResult { compositionId: string; fileId: string; artifactId: string; name: string; revisionId?: string; revisionFailure?: string }
export class MaterializeCancelled extends Error {}
export class MaterializeFailed extends Error {}

const text = (row: Record<string, unknown> | undefined, key: string) => (typeof row?.[key] === "string" ? (row[key] as string) : "");

export async function startMaterialize(execute: MaterializeExecute, request: MaterializeRequest): Promise<string> {
  const { name, format, requestId, ...rest } = request;
  const args: Record<string, unknown> = { name, format, requestId };
  for (const [key, value] of Object.entries(rest)) {
    if (value === undefined || value === "" || (Array.isArray(value) && value.length === 0)) continue;
    args[key] = value;
  }
  const reply = (await execute("composeMaterialize", buildComposeMaterialize(args as Parameters<typeof buildComposeMaterialize>[0])))[0];
  const id = text(reply, "compositionId");
  if (!id) throw new Error("Materializer did not confirm a composition. Check its history before retrying.");
  return id;
}

export async function followComposition(ports: MaterializePorts, compositionId: string): Promise<MaterializeResult> {
  const interval = ports.intervalMs ?? 2000;
  const wait = () => new Promise<void>((resolve, reject) => {
    const timer = setTimeout(resolve, interval);
    ports.signal?.addEventListener("abort", () => { clearTimeout(timer); reject(new MaterializeCancelled("Stopped")); }, { once: true });
  });
  const cancel = async () => {
    await ports.execute("composeCancel", buildComposeCancel({ compositionId, reason: "Stopped by the person" }));
    throw new MaterializeCancelled("Stopped");
  };
  for (;;) {
    if (ports.signal?.aborted) return cancel();
    const row = (await ports.execute("compositionById", buildCompositionById({ compositionId })))[0];
    if (!row) throw new MaterializeFailed("This composition is no longer readable. Its history remains in Materializer.");
    const status = text(row, "status");
    if (status === "failed") throw new MaterializeFailed(text(row, "failureReason") || "Materializer could not finish this file.");
    if (status === "cancelled") throw new MaterializeCancelled("Stopped");
    if (status === "ready") {
      const artifact = (await ports.execute("libraryArtifactForFile", buildLibraryArtifactForFile({ fileId: text(row, "outputFileId") })))[0];
      if (text(artifact, "id")) {
        const result: MaterializeResult = { compositionId, fileId: text(row, "outputFileId"), artifactId: text(artifact, "id"), name: text(artifact, "title") || text(row, "name") };
        if (text(row, "revisionId")) result.revisionId = text(row, "revisionId");
        if (text(row, "revisionFailure")) result.revisionFailure = text(row, "revisionFailure");
        return result;
      }
      ports.onStatus?.("Saved; waiting for its Files entry");
    } else {
      ports.onStatus?.(status);
    }
    try { await wait(); } catch { return cancel(); }
  }
}

export async function materialize(ports: MaterializePorts, request: MaterializeRequest): Promise<MaterializeResult> {
  return followComposition(ports, await startMaterialize(ports.execute, request));
}
```

Export from `sdk/ts/src/client/index.ts`: `export * from "./materialize.js";`.

- [ ] **Step 4: Run tests**

Run: `cd sdk/ts && npm test && npm run build`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add sdk/ts/src/client/materialize.ts sdk/ts/src/client/index.ts sdk/ts/test/materialize.test.ts
git commit -m "feat(sdk): materialize, the one client entry point for creating a file (#6027)"
```

---

### Task 5: VS Code productivity tool on the helper

**Files:**
- Modify: `editors/productivity/src/extension.ts:125-138` (`newMarkdown`)
- Modify: `editors/productivity/src/templateExamples.ts:72-115` (use `startMaterialize` + `followComposition`)
- Test: `editors/productivity/test/newMarkdown.test.ts` (create), `editors/productivity/test/templateExamples.test.ts` (keep passing)

**Interfaces:**
- Consumes: Task 4 exports from `@znasllc-io/memql-sdk-core`.

- [ ] **Step 1: Failing test** `editors/productivity/test/newMarkdown.test.ts`. Extract the command body into an exported function so it is testable: `export async function createMarkdown(api: EditorConnectionAPI, lease: ConnectionLease, name: string, requestId: string): Promise<vscode.Uri>` in a new `editors/productivity/src/newMarkdown.ts`.

```ts
import { test } from "node:test";
import assert from "node:assert/strict";
import { createMarkdown } from "../src/newMarkdown.js";

test("New Markdown creates through the Materializer, once, with a clean name", async () => {
  const calls: { name: string; query: string }[] = [];
  const lease = { domain: "client.example", name: "Client", generation: 1 };
  const api = {
    uploadFile: async () => { throw new Error("New Markdown must not upload"); },
    execute: async (_l: unknown, name: string, query: string) => {
      calls.push({ name, query });
      if (name === "composeMaterialize") return [{ compositionId: "c1" }];
      if (name === "compositionById") return [{ status: "ready", outputFileId: "f1", name: "Plan" }];
      if (name === "libraryArtifactForFile") return [{ id: "a1", title: "Plan.md" }];
      return [];
    },
  };
  const [first, second] = await Promise.all([
    createMarkdown(api as never, lease as never, "Plan.md", "req-1"),
    createMarkdown(api as never, lease as never, "Plan.md", "req-1"),
  ]);
  assert.equal(String(first), "memql-file://client.example/artifacts/a1/Plan.md");
  assert.equal(String(second), String(first));
  const starts = calls.filter(c => c.name === "composeMaterialize");
  assert.ok(starts.every(c => /requestId:\s*"req-1"/.test(c.query) && /format:\s*"markdown"/.test(c.query) && !/statement/.test(c.query)));
});
```

(The server dedupes on `requestId`; the client keeps one in-flight promise per requestId so a double Enter does not even send twice — assert `starts.length === 1`.)

- [ ] **Step 2: Run, verify failure**

Run: `cd editors/productivity && npm test`
Expected: FAIL (module missing).

- [ ] **Step 3: Implement** `editors/productivity/src/newMarkdown.ts`:

```ts
import * as vscode from "vscode";
import { materialize } from "@znasllc-io/memql-sdk-core";
import type { ConnectionLease, EditorConnectionAPI } from "../../vscode/src/connection/api.js";

const inFlight = new Map<string, Promise<vscode.Uri>>();

/** Create a blank Markdown document through the Materializer and return its editor URI. */
export function createMarkdown(api: EditorConnectionAPI, lease: ConnectionLease, name: string, requestId: string): Promise<vscode.Uri> {
  const existing = inFlight.get(requestId);
  if (existing) return existing;
  const stem = name.trim().replace(/\.(md|markdown)$/i, "");
  const run = materialize({ execute: (n, call) => api.execute(lease, n, call) }, { name: stem, format: "markdown", requestId })
    .then(result => vscode.Uri.from({ scheme: "memql-file", authority: lease.domain, path: `/artifacts/${result.artifactId}/${`${stem}.md`.replace(/[\\/]/g, "_")}` }))
    .finally(() => inFlight.delete(requestId));
  inFlight.set(requestId, run);
  return run;
}
```

In `extension.ts`, replace the `files.createFile(...)` line in `newMarkdown` with:

```ts
        const uri = await vscode.window.withProgress({ location: vscode.ProgressLocation.Notification, title: `Creating ${filename}` },
          () => createMarkdown(api, lease, filename, globalThis.crypto.randomUUID()));
        await markdown.show("review", uri);
```

(`api` is the `EditorConnectionAPI` already in scope where `files` is constructed; follow the existing variable.)

In `templateExamples.ts`, replace the `composeMaterialize` call + `follow` loop with `startMaterialize(execute, {...same args..., requestId: globalThis.crypto.randomUUID()})` and a `followComposition({ execute, signal, onStatus: message => progress.report({ message: message === "composing" ? "Studying references and composing the draft" : message }) }, compositionId)` inside the existing `withProgress`, mapping VS Code's cancellation token to an `AbortController`. Keep `resume()` working via `followComposition`. Keep opening the result with `memql.productivity.email`.

- [ ] **Step 4: Run tests**

Run: `cd editors/productivity && npm test`
Expected: PASS including `templateExamples.test.ts`.

- [ ] **Step 5: Commit**

```bash
git add editors/productivity/src/newMarkdown.ts editors/productivity/src/extension.ts editors/productivity/src/templateExamples.ts editors/productivity/test/newMarkdown.test.ts editors/productivity/test/templateExamples.test.ts
git commit -m "feat(productivity): New Markdown and email drafts create through the Materializer (#6028)"
```

---

### Task 6: OS Materializer app on the helper, plus the starting-setup intent

**Files:**
- Create: `clients/os/src/items/materialize.ts` (intent contract)
- Modify: `clients/os/src/apps/materializer/actions.ts:85-125` (use `startMaterialize`)
- Modify: `clients/os/src/apps/materializer/MaterializerApp.tsx:170-190` (consume `compose` intent)
- Modify: `clients/os/src/apps/materializer/ComposerSection.tsx` (accept initial facts)
- Test: `clients/os/test/materializer/intent.test.tsx` (create), existing materializer tests stay green

**Interfaces:**
- Consumes: `startMaterialize` (Task 4).
- Produces (exact, `clients/os/src/items/materialize.ts`):

```ts
export const MATERIALIZER_APP_ID = "materializer";
export const MATERIALIZER_COMPOSE_SECTION = "compose";
export interface ComposeSetup {
  format?: "markdown" | "html" | "txt" | "csv" | "json" | "docx" | "pdf";
  folderId?: string;
  sources?: { kind: "concept_row" | "library_file" | "query"; ref: string; label: string }[];
  outputKind?: "email_template";
  returnTo?: { appId: string; sectionId: string };
}
export function composeIntent(setup: ComposeSetup): Record<string, unknown> { return { compose: setup }; }
export function readComposeIntent(payload: Record<string, unknown>): ComposeSetup | null;
```

`readComposeIntent` validates every field (unknown format -> dropped; sources without string `kind/ref/label` -> dropped; `returnTo` needs both strings) and returns `null` when `compose` is absent.

- [ ] **Step 1: Failing tests** `clients/os/test/materializer/intent.test.tsx`:

```tsx
import { describe, it, expect } from "vitest";
import { composeIntent, readComposeIntent } from "../../src/items/materialize";

describe("Materializer starting setup", () => {
  it("round-trips a valid setup", () => {
    const setup = { format: "markdown" as const, folderId: "folder-1", sources: [{ kind: "library_file" as const, ref: "f1", label: "notes.md" }], returnTo: { appId: "files", sectionId: "browse" } };
    expect(readComposeIntent(composeIntent(setup))).toEqual(setup);
  });
  it("drops unknown formats and malformed sources", () => {
    expect(readComposeIntent({ compose: { format: "exe", sources: [{ kind: "x" }, { kind: "library_file", ref: "f", label: "a" }] } }))
      .toEqual({ sources: [{ kind: "library_file", ref: "f", label: "a" }] });
  });
  it("ignores payloads without a setup", () => {
    expect(readComposeIntent({ compositionId: "c1" })).toBeNull();
  });
});
```

Add a render test next to the existing materializer tests (grep `clients/os/test/materializer` for the harness that renders `MaterializerApp` with an `intent`): deliver `{ id: "i1", payload: composeIntent({ format: "markdown", folderId: "missing" }) }` and assert the Compose section shows Markdown selected and that `consumeIntent` was called with `"i1"`. A folder the folder feed does not contain is not pre-selected (Review Focus 5).

Extend the existing actions test (grep `composeMaterialize` under `clients/os/test`) to assert the call carries a `requestId` string and that two rapid submits with the same facts reuse it.

- [ ] **Step 2: Run, verify failure**

Run: `cd clients/os && npx vitest run test/materializer`
Expected: FAIL.

- [ ] **Step 3: Implement**
  - `items/materialize.ts` as specified above.
  - `actions.ts`: replace `query.composeMaterialize({...})` with `await startMaterialize((name, call) => query.executeNamed(name, call).then(r => r.rows()), { ...same fields..., requestId: requestIdRef.current })`, where `requestIdRef` is a `useRef(crypto.randomUUID())` renewed only after a composition id is returned or `reset()` is called. Following stays on the app's existing live composition feed (OS rule: subscriptions, not polling).
  - `MaterializerApp.tsx`: in the intent effect, `const setup = readComposeIntent(intent.payload)`; when present, switch to the compose section and pass `setup` to `ComposerSection` as `initial`, then `consumeIntent(intent.id)`. Keep the existing `compositionId` intent branch.
  - `ComposerSection.tsx`: on a new `initial` prop, set format, sources and (only if the folder feed contains it) folder. After a successful materialize with `initial.returnTo`, show the existing "Open" action plus a text action "Back to <app name>" that calls `openApp(returnTo.appId, { section: returnTo.sectionId })`.

- [ ] **Step 4: Run tests + typecheck**

Run: `cd clients/os && npm run typecheck && npx vitest run test/materializer test/ask/navigationContract.test.ts`
Expected: PASS. If the navigation contract changes, regenerate with `MEMQL_UPDATE_NAVIGATION=1 npx vitest run test/ask/navigationContract.test.ts` and commit `component/memql/os_navigation.json`.

- [ ] **Step 5: Commit**

```bash
git add clients/os/src/items/materialize.ts clients/os/src/apps/materializer/actions.ts clients/os/src/apps/materializer/MaterializerApp.tsx clients/os/src/apps/materializer/ComposerSection.tsx clients/os/test/materializer/intent.test.tsx
git commit -m "feat(os): Materializer creates through the SDK and accepts a starting setup (#6029)"
```

---

### Task 7: Guard test — one creation path

**Files:**
- Create: `clients/os/test/onePathCreate.test.ts`
- Create: `editors/productivity/test/onePathCreate.test.ts`
- Read first: `clients/os/test/files/onePath.test.ts` (the upload guard this mirrors)

**Interfaces:**
- Consumes: the final call sites from Tasks 5 and 6.

- [ ] **Step 1: Write the OS guard**

```ts
import { describe, it, expect } from "vitest";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";

// New files are created by the Materializer (sdk materialize) only. Uploading bytes
// the person already has is the one other way in, and only from these modules.
const ROOT = join(__dirname, "..", "src");
const CREATORS = /\b(uploadFile|createLibraryFile|composeMaterialize)\s*\(/;
const ALLOWED: Record<string, string> = {
  "items/upload.ts": "the one upload port (POST /artifacts)",
  "apps/files/useUploadTasks.ts": "Files uploads of existing files",
  "apps/files/uploadTree.ts": "folder uploads of existing files",
};

function walk(dir: string): string[] {
  return readdirSync(dir).flatMap(name => {
    const path = join(dir, name);
    return statSync(path).isDirectory() ? walk(path) : /\.(ts|tsx)$/.test(name) ? [path] : [];
  });
}

describe("one creation path", () => {
  it("creates files only through the Materializer or the named upload modules", () => {
    const offenders = walk(ROOT).map(p => relative(ROOT, p)).filter(rel => !(rel in ALLOWED) && CREATORS.test(readFileSync(join(ROOT, rel), "utf8")));
    expect(offenders).toEqual([]);
  });
  it("catches a planted violation", () => {
    expect(CREATORS.test("await query.composeMaterialize({ name })")).toBe(true);
    expect(CREATORS.test("connection.query.createLibraryFile({})")).toBe(true);
  });
});
```

Before finalising `ALLOWED`, run `grep -rnE "\b(uploadFile|createLibraryFile|composeMaterialize)\s*\(" clients/os/src` and add any remaining legitimate upload module (for example the Campaigns audience CSV import) with a one-line reason; any non-upload creator is a bug to move onto `startMaterialize`, not an allowlist entry.

- [ ] **Step 2: Write the productivity guard** with the same shape over `editors/productivity/src`, `CREATORS = /\b(uploadFile|createFile|composeMaterialize)\s*\(/`, and `ALLOWED`:
  - `documents.ts`: "uploads of existing bytes (insert image, review attachments)"
  - `templateExamples.ts`: "uploads reference files the person picked"
  Plus an assertion that `extension.ts` contains no `createFile(` call outside the `insertImage` command block (check by locating `"memql.productivity.markdown.insertImage"` and asserting the only `createFile(` occurrence is after it).

- [ ] **Step 3: Run both; then plant a violation** (temporarily add `query.composeMaterialize({})` to `clients/os/src/apps/files/BrowseSection.tsx`), confirm FAIL, revert.

Run: `cd clients/os && npx vitest run test/onePathCreate.test.ts` and `cd editors/productivity && npm test`
Expected: PASS after revert.

- [ ] **Step 4: Commit**

```bash
git add clients/os/test/onePathCreate.test.ts editors/productivity/test/onePathCreate.test.ts
git commit -m "test: guard the one file-creation path (#6030)"
```

---

### Task 8: Docs and close-out

**Files:**
- Modify: `docs/public/` page that documents the Materializer (grep `composeMaterialize` under `docs/public`) — add blank files, `requestId`, and the described-Markdown handoff.
- Modify: `clients/os/README.md` Materializer section — the starting-setup intent.
- Modify: `clients/os/qa/list-coverage.md` / `qa/loading-and-navigation.md` Materializer rows if behaviour changed.
- Delete: `docs/superpowers/plans/2026-10-11-materializer-front-door.md` (spent plan; per `docs/CLAUDE.md`).

- [ ] **Step 1:** Update the docs above in plain, current-only language (no "previously").
- [ ] **Step 2:** Run `go test ./docs/... -count=1` (front-matter + public boundary gates).
- [ ] **Step 3:** Delete this plan file in the same merge as the last task.
- [ ] **Step 4: Commit**

```bash
git add docs/public/<page>.md clients/os/README.md clients/os/qa/list-coverage.md
git rm docs/superpowers/plans/2026-10-11-materializer-front-door.md
git commit -m "docs: Materializer is the one creation path (#6031)"
```
