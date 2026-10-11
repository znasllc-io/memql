# The Materializer as the one way a file is created

- **Date:** 2026-10-11
- **Status:** approved 2026-10-11; implementation plan in `docs/superpowers/plans/2026-10-11-materializer-front-door.md`
- **Branch:** `docs/materializer-front-door-spec`, cut from `origin/main` at `78ed527f3`
- **Program:** sub-project 1 of 5 in the email, campaigns and materializer rework (see "The program" below)

## Amendments from planning

Found while writing the implementation plan; they refine, not change, the design.

1. **A blank file has no provenance block.** The Markdown writer puts YAML front
   matter at the top of every file, and the editor's empty-document page appears
   only when the source is empty (`editors/productivity/src/markdownView.ts:989`).
   Blank files are therefore written by a separate blank writer (Markdown is
   exactly `"\n"`, JSON `{}`, HTML an empty document, text and CSV empty); their
   history lives on the composition row. A described Markdown request also
   starts from this blank file.
2. **The revision step waits for the Files entry.** A new file's Library index
   row is written by the `indexFileOnCreate` automation after the file is
   created, so `composeRequestRevision` polls for it for a bounded time (30
   attempts, one second apart, configurable) before posting the comment against
   `expectedVersion 1` / `expectedRevision "file:1"`. If the row never appears,
   it records `revisionFailure` and the file stays.
3. **The OS follows compositions live.** The SDK module exposes
   `startMaterialize` and `followComposition` as well as `materialize`. The OS
   Materializer app starts through `startMaterialize` and keeps following on its
   live composition feed, because OS rules prefer subscriptions to polling; the
   editors use the polling `followComposition`.
4. **Names are cleaned on the server.** A trailing extension matching the format
   is stripped (`Plan.md` stays `Plan.md`, never `Plan.md.md`) and a name with a
   slash is refused before anything is written.

## In plain terms

MemQL has two ways to make a new file. The Materializer records what was asked
for, what it was made from and which run made it. The VS Code productivity
tool's "New Markdown" skips all of that: it uploads an empty file and the
document is then written through the Library's revision workflow. The OS has
no way to create a file from scratch at all.

This design makes the Materializer the only way a new file comes into being,
everywhere, without weakening the Markdown writing experience that already
works well:

- **Every new file is a composition**, including a blank one. Uploading a file
  you already have stays an upload.
- **One SDK helper, `materialize`,** is how the OS and both editor extensions
  create a file. A guard test fails the build if new-file creation happens
  anywhere else.
- **A Markdown request with a description is still written by today's Library
  revision workflow** (intent, research, images, a proposal the person
  approves). The Materializer creates the file, then hands the description to
  that workflow, so first drafts are as good as they are today.
- **The Materializer app accepts a starting setup** (format, folder, sources)
  from other apps. "New Markdown file" in Files and "Create new template" in
  the Email wizard use it in later sub-projects.

## The program

The owner's request (2026-10-11) covers five pieces, built in this order
because each later one plugs into an earlier one:

1. **Materializer as the one creation path.** This spec.
2. **Email-template authoring in VS Code with layouts.** A built-in set of email
   layouts (derived from example emails the owner will supply) plus layouts an
   organization saves from its own finished templates. Pick a layout, describe
   the email, AI writes it, edit by hand. Layouts are email-only and modelled
   for email specifically.
3. **Email absorbs Campaigns.** One Email app, campaign creation as one kit
   `Wizard` per `clients/os/DESIGN.md`, a "Create new template" step that opens
   the VS Code tool, and the production visibility fix (`app:email` is granted
   only to owner and developer in `dsl/rbac/seeds.memql:303`, while
   `app:campaigns` reaches every role).
4. **Materializer app redesign.** Mix artifacts, describe what you want, get the
   file, following the OS interface rules.
5. **"New Markdown file" in Files**, which opens the Materializer app.

Each piece gets its own spec, plan and implementation cycle.

## Decisions taken in brainstorming

| Question | Decision |
|---|---|
| Order | Foundation first: 1, 2, 3, 4, 5 |
| Where layouts come from | Built-in set plus organization-saved layouts (sub-project 2) |
| Which formats get layouts | Email only, email-specific model (sub-project 2) |
| Create Markdown from the OS | Opens the Materializer app; the result opens in VS Code |
| Create Markdown from VS Code | Stays in the editor; the Materializer runs behind it |
| Editing after creation | Unchanged: the Library review and revision workflow |
| Blank files | Also go through the Materializer; uploads of existing files do not |
| Approach | Front door plus reuse of the Markdown revision workflow (option A) |

Rejected approaches: porting research, images and approval into the
Materializer's own composer (option B, rebuilds mature machinery and regresses
Markdown quality during the transition), and having the Materializer only
record a composition beside the old path (option C, leaves two creation paths).

## What exists today

- **Materializer.** `builtin composeMaterialize` (`dsl/compose/builtins.memql:28`)
  saves a `v1:compose:composition` and its inputs, then runs
  `composeMaterializeWorkflow` (`dsl/compose/automations.memql:14`). The
  workflow either restores a recipe draft, renders supplied sections, asks the
  composer for a draft (`app/materializer_composer.go:41`, one structured model
  call) or requires a supplied draft, then renders, files the output as a
  `v1:library:file` and completes. With no composer and no draft,
  `composeRequireDraft` fails "there is nothing to render"
  (`integrations/compose/materialize.go:547`). There is no `requestId`; the
  composition id is only stable when the call happens inside a run
  (`materialize.go:180`).
- **VS Code "New Markdown."** `memql.productivity.newMarkdown`
  (`editors/productivity/src/extension.ts:125`) uploads `"\n"` with
  `Documents.createFile` (`documents.ts:33`, a `POST /artifacts` upload) and
  opens Review. The empty page offers "Describe document" and "Write in
  Source". Describing posts a document-level comment
  (`libraryAddDocumentComment`), and "Propose changes" calls
  `libraryRequestDocumentRevision`, which opens the `reviseLibraryDocument`
  work goal (`integrations/library/revision.go:146`,
  `dsl/library/automations.memql:252`).
- **VS Code "template from examples."** Already calls `composeMaterialize`
  with `outputKind: "email_template"` and follows the composition with its own
  two-second loop (`editors/productivity/src/templateExamples.ts:77-115`).
- **Other VS Code uploads.** Insert image (`extension.ts:153`) and review
  attachments (`markdownEditor.ts:226`) upload bytes the person already has.
  They stay uploads.
- **OS.** Files can upload and create folders; it cannot create a file
  (`clients/os/src/apps/files/BrowseSection.tsx:1097-1165`). Cross-app opening
  uses `openApp` with an intent payload (`clients/os/src/system/desks.ts:140`);
  the Materializer already names its hand-off targets in one file
  (`clients/os/src/apps/materializer/handoff.ts`).
- Both `clients/os` and `editors/productivity` depend on
  `@znasllc-io/memql-sdk-core` (`sdk/ts`).

## Design

### 1. Server: blank files, idempotency, and handing Markdown to the revision workflow

All changes are in `dsl/compose/` and `integrations/compose/`.

**Blank requests.** A request with no `statement`, no `draft`, no
`sectionKeys`, no `sources`, no `templateId`, no recipe and no
`outputKind`/`deployableKind` is a blank request. The workflow gains a branch
before drafting: a blank request skips drafting and renders an empty body for
its format, then files and completes as usual. Allowed formats for blank:
`markdown`, `txt`, `html`, `csv`, `json`. A blank `docx` or `pdf` is refused at
request time with "A blank file can be Markdown, text, HTML, CSV or JSON."
(those formats have no useful empty document, and nothing asks for one).

**Idempotency.** `composeMaterialize` gains an optional `requestId string`
argument. When present and the call is not inside a run, the composition id is
derived from (actor user id, `requestId`) the same way `stableMaterializeID`
derives it inside a run. A repeat with the same `requestId` returns the existing
composition's `{compositionId, goalId, runId}` instead of starting a second one.

**Handing Markdown to the revision workflow.** A request with
`format: "markdown"`, a non-empty `statement`, no `draft`, no `sectionKeys`,
no `templateId` and no recipe is a *described Markdown* request. For it:

1. The workflow renders and files an empty Markdown file, exactly as for a
   blank request, and completes the composition (`status: ready`,
   `outputFileId` stamped).
2. A new final step, `composeRequestRevision`, then:
   - posts the statement as a document-level comment on the new file
     (`anchor: {kind: "document", quote: "Entire document"}`), with every
     `library_file` source as a reference attachment;
   - calls `libraryRequestDocumentRevision` against the file's first revision.
3. The step stamps `revisionId` on the composition. If either call fails, it
   stamps `revisionFailure` with the reason and the composition stays `ready`:
   the file is good and is never removed because a later step failed.

`composeRequestRevision` calls `libraryAddDocumentComment` and
`libraryRequestDocumentRevision` **through the engine as named builtins**, not
by importing the Library integration's Go code. The Materializer and the
Library may run on different nodes; the engine's existing routing carries the
call to the node that owns the Library, and the revision's work goal is the
Library's own, so no state is held on the composing node.

Described Markdown with `templateId` or `draft` keeps today's composer path:
the person asked for a specific template or supplied text, and the revision
workflow is for writing from a description.

**Concept changes** (`dsl/compose/concepts.memql`, `composition`):

- `revisionId string` — the Library revision opened for a described Markdown request.
- `revisionFailure string` — why that revision could not be opened, when it could not.

The `materializeArgs` struct and the SDK generated builder gain `requestId`.
Regenerate the SDK (`sdk/ts/src/client/generated_*`) in the same change.

### 2. SDK: `materialize`

A new module in `sdk/ts` (exported from the package root):

```ts
type Execute = (name: string, args: Record<string, unknown>) => Promise<Record<string, unknown>[]>;

interface MaterializeRequest {
  name: string;
  format: "markdown" | "html" | "txt" | "csv" | "json" | "docx" | "pdf";
  statement?: string;
  sources?: MaterializeSource[];
  templateId?: string;
  folderId?: string;
  outputKind?: "email_template";
  accountIds?: string[];
  recipeId?: string;
  requestId: string;          // caller-supplied; the same click retried reuses it
}

interface MaterializeResult {
  compositionId: string;
  fileId: string;
  artifactId: string;
  revisionId?: string;
  revisionFailure?: string;
}

function materialize(
  ports: { execute: Execute; signal?: AbortSignal; onStatus?: (status: string) => void },
  request: MaterializeRequest,
): Promise<MaterializeResult>;

function followComposition(ports, compositionId): Promise<MaterializeResult>;  // resume
```

Behaviour:

1. Calls `composeMaterialize`. No `compositionId` in the reply throws
   "Materializer did not confirm a composition. Check its history before retrying."
2. Polls `compositionById` every two seconds, reporting each status through
   `onStatus`.
3. `failed` throws the composition's `failureReason`. `cancelled` throws a
   `MaterializeCancelled` error the caller can ignore.
4. `ready` resolves `outputFileId` to its artifact with
   `libraryArtifactForFile`; while the artifact index has not caught up, it
   reports "Saved; waiting for its Files entry" and keeps polling.
5. An aborted `signal` calls `composeCancel` with the reason "Stopped by the
   person" and throws `MaterializeCancelled`.

`execute` keeps the helper transport-neutral: the OS passes a function over
`connection.query.executeNamed`; VS Code passes one over
`EditorConnectionAPI.execute(lease, …)`. The helper never imports a transport.

### 3. VS Code productivity tool

- `memql.productivity.newMarkdown` asks for the name as today, then calls
  `materialize({ name, format: "markdown", requestId })` (blank) and opens the
  result in Review. The empty-document page is unchanged; "Describe document"
  is now an edit to a Materializer-created file and stays on the revision
  workflow.
- `TemplateExamples` replaces its private `follow` loop with
  `materialize` / `followComposition`, keeping its progress notification,
  cancellation and "resume the email draft" command.
- `Documents.createFile` remains only for uploads (insert image, attachments).

### 4. OS: the Materializer's starting setup

The Materializer app accepts an `openApp` intent:

```ts
{ compose: { format?: string; folderId?: string; sources?: { kind: string; ref: string; label: string }[]; outputKind?: string; returnTo?: { appId: string; sectionId: string } } }
```

Bare ids only, per the identifiers contract. On receipt the app opens its
composing surface pre-filled with that setup. `returnTo` names where the
person goes back to after the file is created (used by the Email wizard).

This sub-project defines and tests the contract against the **current**
Compose section; sub-project 4 redesigns the surface that receives it, and
sub-projects 3 and 5 are its first senders. The contract's key names and
builder live in one exported module (`clients/os/src/items/materialize.ts`) so
senders never import from `apps/materializer/`.

### 5. Guard test: one creation path

A test in `clients/os/test` (and its twin in `editors/productivity/test`) scans
source for file-creating calls — `uploadFile(`, `createFile(`, the edge upload
port, `createLibraryFile(`, `composeMaterialize(` — and fails when one appears
outside an allowlist:

- **Uploads of existing bytes:** `clients/os/src/items/upload.ts`,
  `clients/os/src/apps/files/useUploadTasks.ts`, `uploadTree.ts`, the
  Campaigns audience CSV import, `editors/productivity/src/documents.ts`
  (`createFile`, called only from insert-image and attachments) and
  `templateExamples.ts` (reference uploads).
- **Creation:** the `materialize` module in `sdk/ts` only. The Materializer
  app's own `composeMaterialize` call (`clients/os/src/apps/materializer/actions.ts:103`)
  moves onto the helper in this sub-project; `composeRunRecipe` stays where it
  is and returns a composition the app follows with `followComposition`.

The edge upload port means the `POST /artifacts` binding behind
`clients/os/src/items/upload.ts`; the scan matches its exported names, not a
URL string.

Each allowlist entry carries a one-line reason. The test is proved against a
planted violation before it lands.

## Failure handling

| Case | What the person sees | What remains |
|---|---|---|
| Request refused (no access, bad format, blank docx/pdf) | The refusal, saying what was refused | Nothing created |
| Run fails | The composition's failure reason | Composition `failed`, retryable from "Materialized" |
| File created, revision request fails | The document opens with a notice that the description was not applied, and "Describe document" available | File, composition `ready` with `revisionFailure` |
| Person cancels | Nothing further | Composition `cancelled`; a blank file already filed is kept |
| Files entry slow to appear | "Saved; waiting for its Files entry" | Polling continues |
| Same request repeated (double click, retry) | The same result | One composition, one file |

## Testing

- **Go, against the CI Postgres image** (not mocks, so a refused write cannot
  pass silently):
  - blank `markdown`, `txt`, `html`, `csv`, `json` each produce a file and a
    `ready` composition with no model call;
  - blank `docx`/`pdf` are refused before a composition is written;
  - described Markdown produces a file, a document comment carrying the
    statement and references, and a `revisionId` whose status is readable via
    `libraryDocumentRevisionStatus`;
  - a failing revision request leaves the file and records `revisionFailure`;
  - a repeated `requestId` returns the same composition;
  - described Markdown with `templateId` still uses the composer.
- **Cross-node:** a `test/clustere2e/` case with the composition on one node
  and the Library on another. It fails against a direct Go call and passes with
  the engine-routed builtins.
- **SDK** (`sdk/ts` tests, fake `execute`): status sequence, failure reason,
  cancellation calls `composeCancel`, waiting for the Files entry, abort,
  `followComposition` resume.
- **VS Code:** `newMarkdown` creates through `materialize`, never
  `createFile`; `templateFromExamples` uses the shared follow; the gallery
  screenshots for the empty document are unchanged.
- **OS:** the intent contract round-trips into the current Compose section.
- **Guard test:** passes on the tree, fails on a planted violation.

## Out of scope

- Email layouts and the layout picker (sub-project 2).
- The Email and Campaigns merge, the campaign wizard, and the `app:email` grant
  (sub-project 3). The grant fix may ship early as its own small change once
  the owner names the roles that should hold it.
- Redesigning the Materializer app (sub-project 4).
- "New Markdown file" in Files (sub-project 5).
- Moving edits after creation onto the Materializer. Edits stay on the Library
  revision workflow by decision.
