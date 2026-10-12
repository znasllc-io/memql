# The Materializer as the one way a file is created

- **Date:** 2026-10-11
- **Status:** approved 2026-10-11 and built (memql#6023, tasks #6024 to #6031); this record is amended below to what shipped. The implementation plan was deleted with the epic's merge.
- **Branch:** written on `docs/materializer-front-door-spec`, cut from `origin/main` at `78ed527f3`; built on `epic/materializer-front-door`
- **Program:** sub-project 1 of 5 in the email, campaigns and materializer rework (see "The program" below)

## Amendments from planning and implementation

Found while writing the implementation plan and while building it; they refine,
not change, the design.

1. **A blank file has no provenance block.** The Markdown writer puts YAML front
   matter at the top of every file, and the editor's empty-document page appears
   only when the source is empty (`editors/productivity/src/markdownView.ts:989`).
   Blank files are therefore written by a separate blank writer (Markdown is
   exactly `"\n"`, JSON `{}`, HTML an empty document, text and CSV empty); their
   history lives on the composition row. A described Markdown request also
   starts from this blank file.
2. **The revision step waits for the Files entry.** A new file's Library index
   row is written by the `indexFileOnCreate` automation after the file is
   created, so the hand-off cannot post its comment until the artifact exists.
   The server no longer owns that step (amendment 5): the SDK's `materialize`
   already waits for the artifact ("Saved; waiting for its Files entry") before
   it calls `handOffDescription`, which posts the comment against
   `expectedVersion 1` / `expectedRevision "file:1"`.
3. **The OS follows compositions live.** The SDK module exposes
   `startMaterialize` and `followComposition` as well as `materialize`. The OS
   Materializer app starts through `startMaterialize` and keeps following on its
   live composition feed, because OS rules prefer subscriptions to polling; the
   editors use the polling `followComposition`.
4. **Names are cleaned on the server.** A trailing extension matching the format
   is stripped (`Plan.md` stays `Plan.md`, never `Plan.md.md`) and a name with a
   slash is refused before anything is written ("Enter a name without slashes.";
   a name with nothing left once the extension is dropped is refused with "Enter a
   name for the file."). A stem the planner's triage
   chooses is not a person's typing, and a refusal there would come after the
   drafting steps had run, so the planner turns path separators in it into
   hyphens before it calls the Materializer.
5. **The hand-off to the revision workflow is made by the person's client, not
   by the run.** The Library refuses a revision request made from inside a work
   run (`libraryRequestDocumentRevision` answers "a person must prepare the
   document revision request", `integrations/library/revision.go`; the
   revision-item changes in `revision_items.go` carry the same guard). That is a
   deliberate boundary: a running job must not request or approve revisions on a
   person's behalf. So the server files a described Markdown request blank and
   keeps its statement on the composition, and the SDK's `handOffDescription`
   posts the description as the person: a document-level comment with the
   `library_file` sources as attachments (those the Library accepts, amendment
   7), then `libraryRequestDocumentRevision`, returning `revisionId` or
   `revisionFailure` to the caller. There is no
   `composeRequestRevision` step and there are no `revisionId` or
   `revisionFailure` fields on the composition. The cross-node test (task 3) is
   dropped: the hand-off is the same client-routed builtin sequence VS Code's
   "Describe document" already uses, so there is no new cross-node state to
   prove.

   The same boundary decides which requests are filed blank. Only a request a
   person's own client makes is: a call made inside a work run (the planner's
   file delivery, an agent's `composeFile`) says what to write in its statement
   and has no client to hand the description on, so it keeps the composer.

   **Only Library files travel with a description.** The revision workflow reads
   Markdown and images and cannot read the graph, so a Markdown request is a
   described request only when every source is a `library_file` (or there are
   none). A request with a `concept_row` or `query` source keeps the composer,
   which reads the records. The server's `describedMarkdown()` and the SDK's
   `isDescribedMarkdown` carry the same rule and move together.
6. **The hand-off's request ids are `<requestId>-comment` and
   `<requestId>-revision`.** The Library accepts only `[a-zA-Z0-9_-]{8,120}` as a
   request id, so a separator such as a colon would make every real hand-off
   refuse; `revisionId` on the result is `<requestId>-revision`. The comment's
   own id comes from the comment's receipt, because the review listing withholds
   each comment's request id. A request id used with a description is therefore
   at most 111 characters, and `startMaterialize` refuses a described request
   whose trimmed id is outside that, before the file is made. A retry of the
   hand-off must send the same inputs: the Library refuses a changed comment
   under an old request id.
7. **`handOffDescription` attaches what the Library accepts and reports the
   rest.** The Library takes Markdown (non-empty, up to 32 KiB) and images (PNG,
   JPEG, GIF, WebP, up to 8 MiB) as review attachments, eight at most, 16 MiB in
   all and 64 KiB of Markdown in all, and refuses the whole comment for anything
   else. The hand-off applies those rules from each file's name and size, attaches
   what fits, opens the revision with it, and returns the labels of every other
   reference (not a file, not found, a type or size the Library refuses) as
   `unattached` on the result, for the caller to show. When the Library refuses
   the comment with a sentence about attachments (every one says "attach"), the
   comment is sent once more without them and all of them are reported. A Go test
   holds these limits and that wording to the Library's own, and the failure text
   a person sees never carries the construct name `executeNamed` puts in front of
   a refusal, nor the engine's frame (amendment 11).
8. **A folder the caller cannot use is refused by the server.** `composeMaterialize`
   reads `folderId` as the caller, fresh, and refuses one that is not readable
   (someone else's, deleted, no such id) or that is archived, with "That folder
   isn't available. Choose another folder.", before anything is written. The OS
   sends the folder a starting setup claims and shows that sentence as worded;
   it does not check the folder first, which would turn the refusal into a file
   filed quietly at the top. A repeat of an accepted request finds its
   composition and is not refused because the folder was archived since.
9. **A stop never deletes a file that was already made.** It stays in Files, and
   the client opens it when it is already in Files at the moment of the stop. Once
   the composition is ready there is nothing to cancel, so `materialize` returns
   the file; for a described request it skips the hand-off and returns
   `revisionFailure` "Stopped before the description was sent.". A stop that races
   the composition finishing is the same: `composeCancel` cancels nothing that has
   finished and answers `cancelled: false`, and the composition is read again
   before the file is given up (a composition that failed meanwhile reports why).
   A stop before the file exists cancels the composition and throws
   `MaterializeCancelled`, and so does a stop while a ready file's Files entry is
   still missing: there is no artifact to hand back, and the file is in Files a
   moment later. VS Code's New Markdown notification is cancellable on this basis.
10. **The `composeFile` agent tool cannot send an empty statement, and a test holds
    it.** The tool calls `composeMaterialize`, and a statement that is empty or only
    spaces never reaches the Materializer: the engine's call contract refuses a
    blank value for a string field of a builtin when the call is parsed
    (`composeMaterialize() statement cannot be empty`), and a tool whose handler is
    a function reaches its builtin by rendering that call and executing it. A
    tool field cannot say "not empty" itself (the annotation set has only
    `required`), so the guard is a test that runs the shipped tool through the
    engine's `ExecuteTool`, as the agent tool loop does, with the Materializer's
    capability replaced by a recorder: the empty, whitespace-only and missing
    statements are refused before the recorder runs, and a real statement reaches
    it as sent. A person's client still leaves the statement out to ask for a
    blank file.
11. **What a person reads is one sentence, and a finished file says what it is**
    (the whole-branch review's fixes). A refusal reaches a client framed: the call's
    name in front (`executeNamed`) and the server's "MemQL engine failed to execute
    query. Details: " around it. The SDK's `refusalSentence`, beside `executeNamed`,
    takes both off, and a leading Go package name; `startMaterialize` throws and
    `handOffDescription` reports in those words, and the OS shows every refusal
    through it, reads included. The OS composer checks a name as it is typed with the
    SDK's `fileNameProblem`, the rule and words of VS Code's New Markdown, which VS
    Code's New Markdown and Create Email from Examples now use too; each problem has
    its own sentence ("Enter a file name.", "Enter a file name, without a folder
    path.", "Use a name up to 150 characters.", "Enter a name before the
    extension."), and VS Code's diagnostics keep a refusal's frames through the
    error's cause. The OS renders a composition's `provenanceNote` (only the stamp step knows
    why a file carries no provenance) and says what a finished file is: "This file was
    created blank."; for a described one, "Your description was sent to be written.
    Open the file to review and approve the draft." once the window has sent it, the
    failure notice alone when it could not, and "This file was created from a
    description. Open it to continue." when the window does not know; a file a model
    wrote keeps "The composed text is in the file." A reference left off the hand-off
    is named in its own notice, "This reference wasn't attached.", because the SDK's
    entry is a label that carries no reason. Save as recipe is absent where the
    recipe could not run. `followComposition` makes its first look 300 ms after the
    start and the rest an interval apart, and VS Code's New Markdown shows each status
    in its notification. `composeCancel` decides on a fresh read, so a stop on one
    replica cannot cancel a composition another replica has finished.

## In plain terms

When this was designed, MemQL had two ways to make a new file. The Materializer
recorded what was asked for, what it was made from and which run made it. The VS
Code productivity tool's "New Markdown" skipped all of that: it uploaded an empty
file and the document was then written through the Library's revision workflow.
The OS had no way to create a file from scratch at all.

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
  approves). The Materializer creates the empty file, and the person's client
  then hands the description to that workflow, so first drafts are as good as
  they are today.
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

## What existed when this was designed

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

### 1. Server: blank files, idempotency, and described Markdown

The Materializer changes are in `dsl/compose/` and `integrations/compose/`; the
planner's file stems are normalised in `integrations/planner/`.

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

**Described Markdown, handed to the revision workflow by the client.** A
request with `format: "markdown"`, a non-empty `statement`, no `draft`, no
`sectionKeys`, no `templateId`, no recipe, no `outputKind` and no
`deployableKind` is a *described Markdown* request. Library files may come with
it: they are references for the revision workflow, passed on by the client. A
`concept_row` or `query` source does not: the revision workflow cannot read the
graph, so a request with one keeps the composer, which reads the records
(amendment 5). For a request a person's client makes:

1. The workflow renders and files an empty Markdown file, exactly as for a
   blank request (no model call, no front matter, no source package), and
   completes the composition (`status: ready`, `outputFileId` stamped). The
   composition keeps the `statement`, so the request is on record even if the
   client never hands it on, and the file's Library summary reads "Created
   blank."
2. The person's client then hands the description to the revision workflow,
   as the person. The SDK's `handOffDescription` (section 2) posts the
   statement as a document-level comment on the new file
   (`anchor: {kind: "document", quote: "Entire document"}`), with the
   `library_file` sources the Library accepts as reference attachments (the rest
   come back as `unattached`, amendment 7), and calls
   `libraryRequestDocumentRevision` against the file's first revision. It
   returns `revisionId`, or `revisionFailure` with the reason, and never
   throws: the file is good and is never removed because a later step failed.

The hand-off is the client's because the Library refuses a revision request
made from inside a work run (amendment 5). That is also why only a request a
person's client makes is filed blank: the server decides this once, when it
accepts the request, and saves the decision with the request so every replica
that executes the run agrees. A call made inside a work run keeps the composer.

Described Markdown with `templateId` or `draft` keeps the composer path: the
person asked for a specific template or supplied text, and the revision workflow
is for writing from a description.

**Folder.** A `folderId` the caller cannot read, or one in the Bin, is refused
before anything is written (amendment 8).

**Concept changes.** None. The composition already keeps `statement`; the
outcome of the hand-off (`revisionId` or `revisionFailure`) goes back to the
caller of `handOffDescription` and is not stored on the row (amendment 5).

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
  name: string;                 // the name the cluster filed it under
  revisionId?: string;
  revisionFailure?: string;
  unattached?: string[];        // labels of references the Library could not attach
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
2. Polls `compositionById`, reporting each status through `onStatus`: the
   first look 300 ms after the start, the rest every two seconds.
3. `failed` throws the composition's `failureReason`. `cancelled` throws a
   `MaterializeCancelled` error the caller can ignore.
4. `ready` resolves `outputFileId` to its artifact with
   `libraryArtifactForFile`; while the artifact index has not caught up, it
   reports "Saved; waiting for its Files entry" and keeps polling. A composition
   that is not terminal is also checked against its run, and a run that is
   failed, cancelled or abandoned ends the follow with the run's message, so an
   abandoned run is not waited on for ever.
5. An aborted `signal` before the composition is ready calls `composeCancel`
   with the reason "Stopped by the person" and throws `MaterializeCancelled`,
   unless the cluster answers that the composition had already finished
   (`cancelled: false`), in which case it is read again and handled as below.
   After it is ready nothing is cancelled and the file is kept (amendment 9).
6. For a described Markdown request, once the artifact resolves, `materialize`
   calls `handOffDescription({ artifactId, statement, sources, requestId })` and
   returns its `revisionId` or `revisionFailure` (and `unattached`) on the
   result. It posts the comment and requests the revision as the person
   (amendments 5 to 7) and never throws. `followComposition` alone never hands
   off: the OS follows on its own live feed and calls `handOffDescription`
   itself.

The module also exports `startMaterialize(execute, request)`, which opens the
composition and returns its id (sending only the fields `MaterializeRequest`
declares, and refusing a request with no request id), and
`isDescribedMarkdown(request)`, the one statement of which requests are described
so that a caller following its own feed decides as the server does. A request id
is fresh for every person action; a repeat with the same id after a failed first
attempt returns that failed composition by design.

`execute` keeps the helper transport-neutral: the OS passes a function over
`connection.query.executeNamed`; VS Code passes one over
`EditorConnectionAPI.execute(lease, …)`. The helper never imports a transport.

### 3. VS Code productivity tool

- `memql.productivity.newMarkdown` asks for the name as before (a name that is
  only an extension is refused as it is typed), then calls
  `materialize({ name, format: "markdown", requestId })` (blank) in a
  notification the person can stop (amendment 9) and opens the result in Review.
  The empty-document page is unchanged; "Describe document" is an edit to a
  Materializer-created file and stays on the revision workflow.
- `TemplateExamples` starts with `startMaterialize` and replaces its private
  `follow` loop with `followComposition`, keeping its progress notification,
  cancellation and "resume the email draft" command.
- `Documents.createFile` remains only for uploads (insert image, attachments).

### 4. OS: the Materializer's starting setup

The Materializer app accepts an `openApp` intent:

```ts
{ compose: { format?: string; folderId?: string; sources?: { kind: string; ref: string; label: string }[]; outputKind?: string; returnTo?: { appId: string; sectionId: string } } }
```

Bare ids only, per the identifiers contract. The window opens on the `composer`
section with the form pre-filled from the setup, which is validated on the way in
(`readComposeIntent` keeps what it can use and drops the rest). The setup lives and
dies with that form: the first file made from it uses up its folder and its
return target. `returnTo` names where the person goes back to after the file is
created, as a "Back to <app>" act beside "Open the file" when the shell holds that
app. The folder is sent as claimed and the server's refusal is shown (amendment 8).
The app starts through `startMaterialize` and follows its own live composition
feed; for a described file it calls `handOffDescription` once the composition is
ready and the file's Files entry exists on the artifact feed, with the request id
the file was started with, and shows what became of the description in place (the
notices are announced by a separate always-mounted live region).

This sub-project defines and tests the contract against the **current**
Compose section; sub-project 4 redesigns the surface that receives it, and
sub-projects 3 and 5 are its first senders. The contract's key names and
builder live in one exported module (`clients/os/src/items/materialize.ts`) so
senders never import from `apps/materializer/`.

### 5. Guard test: one creation path

A test in `clients/os/test/onePathCreate.test.ts` and its twin in
`editors/productivity/test/onePathCreate.test.ts` scan every source file under
`src` and enforce three rules. Every rule reads every file whole, and no part of
a file is exempt.

1. **Raw file-creating constructs are banned everywhere, with no allowlist.**
   `composeMaterialize`, `createLibraryFile`, `createArtifact` and
   `createGeneratedOutput` may not appear as a call, as a quoted construct name
   (`executeNamed("composeMaterialize", ...)`) or through the generated builder
   (`buildComposeMaterialize(`). The SDK helpers are the one caller of
   `composeMaterialize` and live outside the scanned trees.
2. **Nothing builds a `File` out of nothing.** `new File(` is banned, so an
   allowlisted uploader cannot send fabricated bytes (`provider.upload(new File(["\n"], "Untitled.md"))`).
3. **Uploads only in the allowlist.** In the OS an upload is `.upload(` (the edge
   upload port), `uploadFile(` or `createFile(`; in the editor it is `uploadFile(`
   or `createFile(`. A module outside the allowlist that uploads fails.

The allowlists name only modules that send bytes the person picked or dropped,
each with a one-line reason:

- **OS:** `apps/campaigns/AudiencesSection.tsx` (the audience CSV import),
  `apps/files/uploadTree.ts` (a dropped folder), `apps/files/useUploadTasks.ts`
  (files picked or dropped in Files), `apps/training/useUploads.ts` (files
  dropped in Training) and `chrome/Desktop.tsx` (files dropped on the desk).
- **Productivity:** `documents.ts` (insert image and review attachments),
  `insertImage.ts` (the open-dialog picture), `markdownEditor.ts` (review
  attachments) and `templateExamples.ts` (reference files).

An allowlisted module must still upload, or its entry is stale and fails, because
a stale entry would exempt whatever is added to that file next. The patterns are
proved against planted violations, and against the code the OS and the editor ran
before they moved onto the Materializer. A new version or a branch of
a document that already exists (`appendDocumentVersion`,
`libraryForkDocumentVersion`, which creates an independent document branch from a
snapshot) is outside the invariant on purpose: it copies what the person already
has, and edits stay on the Library revision workflow. The Materializer app's
creation (`startMaterialize`) and `composeRunRecipe` are not uploads, and are not
constructs the rules ban.

## Failure handling

| Case | What the person sees | What remains |
|---|---|---|
| Request refused (no access, bad format, blank docx/pdf) | The refusal, saying what was refused | Nothing created |
| Run fails | The composition's failure reason | Composition `failed`, retryable from "Materialized" |
| File created, the client's hand-off to the revision workflow fails | The document opens with a notice that the description was not applied, and "Describe document" available | The empty file, and the composition `ready` with its `statement` |
| Person cancels before the file exists | Nothing further | Composition `cancelled` |
| Person cancels after the file exists | If the file is already in Files when they stop, the client opens it, with the description marked as not sent; otherwise nothing opens | The file stays in Files either way; no hand-off is made |
| Files entry slow to appear | "Saved; waiting for its Files entry" | Polling continues |
| Same request repeated (double click, retry) | The same result | One composition, one file |

## Testing

- **Go, against the CI Postgres image** (not mocks, so a refused write cannot
  pass silently):
  - blank `markdown`, `txt`, `html`, `csv`, `json` each produce a file and a
    `ready` composition with no model call;
  - blank `docx`/`pdf` are refused before a composition is written;
  - described Markdown is filed as the empty page with its statement on the
    composition, no model call and no source package, executed on a node other
    than the one that accepted it; the same with `library_file` sources;
  - a repeated `requestId` returns the same composition;
  - described Markdown with `templateId` still uses the composer, and so does
    a described request made inside a work run.
- **Cross-node:** no separate case. The hand-off is client-routed (amendment 5),
  and the accept-on-one-node, execute-on-another path is covered by the Go
  cases above.
- **SDK** (`sdk/ts` tests, fake `execute`): status sequence, failure reason,
  cancellation calls `composeCancel`, waiting for the Files entry, abort,
  `followComposition` resume; `handOffDescription` posts the comment (with
  attachments) and then requests the revision, a failure comes back as
  `revisionFailure` without throwing and without the construct name or the
  engine's frame, and a retry
  reuses the same request ids; a stop after the file exists keeps it; a
  described request whose id the hand-off would refuse is refused before it is
  sent.
- **Go, the Library's limits:** a test holds the SDK's copy of the attachment
  limits, image types and the word "attach" to the Library's own, and every
  attachment refusal the Library words says "attach".
- **Go, the `composeFile` tool:** the shipped tool is run through the engine's
  `ExecuteTool` with an agent's role and the Materializer's capability recorded:
  an empty, whitespace-only or missing statement is refused before the
  Materializer runs, and a real one reaches it as sent.
- **VS Code:** `newMarkdown` creates through `materialize`, never
  `createFile`, in a notification that can be stopped, and refuses an
  extension-only name as it is typed; `templateFromExamples` uses the shared
  follow; the gallery screenshots for the empty document are unchanged.
- **OS:** the intent contract round-trips into the current Compose section; the
  description hand-off, its notices and their live region.
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
