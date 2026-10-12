# The Materializer app: mix things, describe it, get the file

- **Date:** 2026-10-12
- **Status:** design for sub-projects 4 (memql#6034) and 5 (memql#6035); not built yet
- **Branch:** `epic/materializer-app`, cut from `origin/main` at `386e5af39`, which contains sub-project 1 (memql#6051)
- **Program:** sub-projects 4 and 5 of 5 in epic memql#6022
- **Builds on:** `2026-10-11-materializer-front-door-design.md` (sub-project 1). Its section 4 (the starting setup), amendments 5 to 11 and its guard test "one creation path" are binding here.
- **Plan:** `docs/superpowers/plans/2026-10-12-materializer-app.md`

## In plain terms

- **Making a file is the shell's one wizard, Make a file.** Its steps read like a
  recipe: what kind of file, what to make it from, what it should say, its name and
  its folder. Every step but the name starts with a sensible answer, so a blank file
  is a name and one press, and the rail shows the whole recipe before anything is
  made.
- **From is where you mix.** Your Library files and your records sit in one tray
  with a search; what you pick stands above the tray as cards. **Describe** is one
  text box. Both are optional.
- **The folder is visible and yours to change.** A file made from Files starts in the
  folder you were looking at; the Folder step shows it and lets you choose another.
- **After Make the file, the same wizard shows the cluster at work**, with Stop and
  Leave. When the file exists it has a page of its own: what it was made from, what
  you asked for, which model wrote it, and **Open in editor**. A file the cluster
  files blank (a blank file, or a Markdown file you described) opens in the browser
  editor by itself.
- **The app's tabs become its nouns:** Materialized (what was made), Templates and
  Recipes. Make a file is reached from the plus on Materialized, from Start over, and
  from other apps.
- **Files gains New Markdown file** in its Add menu. It opens Make a file with Markdown
  and the current folder already answered, goes back to Files when you are done, and
  the file opens in the editor.

## What exists now

Read on `origin/main` at `386e5af39`, after sub-project 1.

- **Sections.** Compose (`composer`, first, so the window opens there), Materialized,
  Templates, plus Logs and Settings as header icons
  (`clients/os/src/apps/materializer/settings.ts:24-32`). The manifest needs `ai` and
  `storage` for every section (`apps/registry.tsx:669-680`, `MATERIALIZER_REQUIRES`).
- **Compose** (`ComposerSection.tsx`, 751 lines) is three columns over a provenance
  chain and an action bar:
  - *Sources* offers whole record types only (`composableConcepts`, picked as a
    `query` source with a live count from `composeResolveSources`). Library files reach
    the composer only inside a starting setup.
  - *The brief* is Organization (required), Name (checked as typed with the SDK's
    `fileNameProblem`) and a one-line "What do you want made?" (`ComposerSection.tsx:611-635`).
  - *Target* is Kind of file, Template and "Deploy it as" (a package kind for
    Deployables), and a standing "Not offered yet" box naming audio and video
    (`ComposerSection.tsx:683-750`).
  - There is no folder control: a setup's folder is held in form state and sent.
  - Materialize is offered only with a description or a source (`acts.ts:79`), so the
    OS cannot make a blank file; `docs/public/operate/memql-os.md` says so.
  - Once a composition exists the same columns turn into its record ("settled"), the
    Head gains "Compose another" (`ComposerSection.tsx:277-286`) and the bar carries
    the acts (`acts.ts`).
- **Materialized** (`MaterializedSection.tsx`) is a `LiveList` of compositions. A row
  opens the composition in the Compose tab (`MaterializerApp.tsx:190-195`), so the
  detail lives under a peer tab with no way back to the list. Its Refine holds a raw
  `<select>` (`MaterializedSection.tsx:93`).
- **Templates** (`TemplatesSection.tsx`) is templates and recipes in one scroller. "Bind
  a file" opens an inline form above the list (`TemplatesSection.tsx:77-91`, `195-262`);
  Archive, Restore and Run it again are row buttons; recipes carry a standing caption.
  Empty copy shows while the reads are still loading.
- **Settings** (`MaterializerApp.tsx:411-526`) is titled "Materializer settings", uses
  `Check` for on/off preferences, explains an absent spending control, and closes with
  a caption about browser storage.
- **Feeds.** Compositions, templates and recipes seed one page of 50
  (`useCompose.ts:60,79,100,130` with `paged: false`; `dsl/compose/queries.memql`
  paginates 50), so the Head count stops at 50. `idTail` parses canonical ids on the
  client (`MaterializerApp.tsx:405`).
- **Kept from sub-project 1, and reused here:** the starting-setup contract
  (`clients/os/src/items/materialize.ts`), request ids per ask (`actions.ts`
  `useMaterialize`), the description hand-off on the live feeds
  (`descriptionHandOff.ts`), the finished-file captions and notices with their
  always-mounted live region (`words.ts`, `ComposerSection.tsx:360-379`), and the
  dead-run join (`runState.ts`).
- **Files.** The Add menu holds Upload files and New folder, both landing in the folder
  being looked at (`apps/files/BrowseSection.tsx:398-401`, `1097-1164`). "Open in
  Materializer" sends `{compositionId}` to the `composer` section
  (`BrowseSection.tsx:438-458`, `Inspector.tsx:319-340`). No app sends a starting setup.
- **Opening a file.** `openCheckedArtifact` reserves a browser-editor tab inside the
  press, reads the file, then navigates the tab or hands off to VS Code or Cursor
  (`items/openFile.ts`, `items/editorPreference.ts`, `items/vscode.ts`). A refused
  reservation throws "Allow this app to open a new tab, then open the file again."
  Campaigns' New template already reserves the tab before an asynchronous create and
  opens the editor afterwards (`apps/campaigns/TemplatesSection.tsx:64-85`).
- **Kit.** `Wizard`, `ActionBar`, `Rail` (its `skipped` state draws a minus),
  `ChoiceStack`, `RecordRow` (`selected` for a choice), `EmptyState`, `Switch`,
  `RecordListSkeleton`. `TextArea` is promoted to the kit by the email work (email/os
  commit `e78b0ac8c`) and is not on main yet.

## Design

### 1. The app's shape

Tabs are the app's nouns (DESIGN.md, "A tab is a different kind of thing"); Make a file
is a drill-down, not a tab.

| Section id | Shown as | What it holds |
|---|---|---|
| `materialized` | tab "Materialized", first, so the window opens here | The list of compositions. A row opens its page when it is finished (ready, failed or stopped) and its wizard at the making stage while it is still being made. |
| `composer` | no tab; `parent: "materialized"`, named "Make a file" | The Make a file wizard. Reached from the plus on Materialized, a starting setup, Start over, Run it again and a row still being made. The id stays `composer` because it is the starting-setup contract (`MATERIALIZER_COMPOSE_SECTION`). |
| `templates` | tab "Templates" | The template list; a row opens its page; the plus runs Add a template. |
| `recipes` | tab "Recipes" (new) | The recipe list; a row opens its page, which runs it again. Recipes are saved from a finished file, so there is no plus. |
| `logs`, `settings` | header icons | Unchanged. |

A page replaces the list that opened it (rule 11), with the list kept mounted and
hidden so its scroll and filters survive, and publishes `<list> > <name>` and Back to
the list to the window's trail. Nothing is drawn beside a list. One action bar per page
(rule 12).

### 2. Make a file

#### 2.1 The steps

| Step | Asks | Appears | Answered by | Answer on the rail | Starts as |
|---|---|---|---|---|---|
| Kind of file | What comes out. | always | choosing; moves on | "Markdown", "Static site package", "Email template" | the setup's format, else Settings' "Kind of file to start on" (Markdown) |
| Template | How it looks. | only when a live template makes this kind | choosing; moves on | the template's name, "No template" | No template |
| From | What to make it from. | always | picking in the tray (optional) | "Nothing picked", one name, "A and B", "A, B and 3 more" | the setup's sources |
| Describe | What it should say. | always | typing (optional) | the first line, cut at a word within 60 characters, then "…"; "Not described" | empty |
| Name | What to call it. | always | typing | the filed name: "Q3 summary.md" | empty |
| Folder | Where it is filed. | always | choosing; moves on | "Library", the folder's name, "Unavailable folder" | the setup's folder, else Library |
| Organization | Who it is for. | an email template; or a draft that already carries one (Start over of an older record) | choosing; moves on | the organization's name, "No organization" | the default organization for an email template (`useDefaultOrganization`); the record's otherwise |
| File | What you get. | always, last; the cluster's step | the cluster | the filed name, once made | dimmed until Make the file |

The rail's marks (`kit` `Rail`): an answered step is `done`; an optional step that has
been passed with nothing in it is `skipped`, with its "nothing" answer, so the rail
tells the truth about what the file will be made from; a step still waiting for the
person is `waiting`; the open step is `open`; File is `ahead` until the press and
`current` while the cluster works. Every person's step is reachable from the start:
nothing depends on an earlier answer except whether Template and Organization appear.

#### 2.2 Which step opens

The wizard opens on the first step still waiting for the person:

- **From the plus**, From and Describe wait, so it opens on From: the plus is for
  making something from things.
- **From another app's starting setup**, the sender has answered the kind of file and
  the folder. From and Describe start as passed with nothing (From is answered when the
  setup carried sources), so it opens on Name: the one thing left is what to call it.
- **From Start over**, every step is answered from the record, so nothing is waiting.
  A wizard with nothing waiting opens on Describe, the answer most often changed before
  trying again.

A choice step moves on by itself to the next step still waiting. A typing step moves on
with Next (or Enter in Name). Any line of the rail opens its step.

#### 2.3 The floor

The bar on the window's bottom edge says what the file needs, first match wins:

| What is missing | State | Detail |
|---|---|---|
| a name that can be used | Name the file | (the name's own problem is said beside the field, as now) |
| something to make it from, for a kind that cannot start blank, or with a template chosen | Describe it or pick what to make it from | "A blank file can be Markdown, text, HTML, CSV or JSON." or, with a template, "A template needs something to fill it." |
| an organization, for an email template | Choose an organization | |
| Inference, for a file a model writes or a description, when readiness reports it not set up (not yet known, or set up on some nodes, does not block) | Making this needs Inference | "An owner or developer can set it up." when this person cannot reach Settings > Vendors |
| nothing | Ready to make <filed name> | "It starts blank." / "You review its draft in the editor." / "Only Markdown files and pictures go with a description." / "A model writes it." (section 2.10) |

Acts, at most three, primary last, one button (rule 12, "One wizard for adding"):

- **Cancel** (text): the way out. Nothing has been written, so it simply goes: the
  draft is discarded and the window returns to the app that sent the person (a
  setup's `returnTo` that the shell holds and the person may open), else to
  Materialized.
- **One forward act** (primary), absent when none is legal: **Make the file** when the
  floor says Ready; **Set up Inference** when Inference is what is missing and this
  person can reach its settings (it opens Settings > Vendors through `moduleActFor`);
  otherwise **Next** while a later step is still waiting.

Example, from the plus: From is open, the bar reads "Name the file" with Cancel and Next;
Next opens Describe; Next opens Name; once the name is usable the forward act is Make the
file. From Files: Name is open, and typing a usable name turns the bar into "Ready to
make Meeting notes.md · It starts blank." with Make the file.

A refusal of the start is a notice over the steps: "The file was not made." with the
cluster's sentence as its detail, through `refusalSentence` (amendment 11). The draft
and its request id are kept, so pressing again is the same ask (sub-project 1's request
id rules are unchanged).

#### 2.4 Kind of file

One `ChoiceStack` (prose voice), in this order: Markdown, Web page, Plain text,
Spreadsheet (CSV), Data (JSON), Word document, PDF, then Single-page app package,
Static site package and Shopify storefront package, each package carrying the
description "A package for Deployables". "Email template" (description "Opens in the
email editor") is an option only when the draft is one, which only a sender's setup
makes.

A package is one choice, not a format crossed with a "Deploy it as" select: the server
renders every package's page as HTML whatever format is sent (`renderDeployable`,
`integrations/compose/materialize.go:1002`), so the old two-select form offered
combinations that meant nothing. A package sends `format: "html"` and its
`deployableKind`. Choosing a document kind drops an email template (as now).

#### 2.5 From: picking what to make it from

- **Picked things stand at the top as cards**: a glyph for the kind of thing, the
  name, one quiet fact and a remove control (`IconButton` "Remove <name>"). A file's
  fact is its kind ("Markdown", "Picture", "Text", "CSV", "JSON", "Web page", "ZIP");
  a record type's is its live row count from `composeResolveSources` ("12 rows"), an
  em dash until the count arrives, or the cluster's problem sentence when it reported
  one. A card arrives with a short rise (none under reduced motion).
- **Below, the tray**: a search field (label "Search your files and records",
  placeholder "Search"), then two groups under Subheads:
  - **Files**: the person's Library files the Materializer can read, newest first;
    the 20 newest when nothing is searched, every match when something is. With more
    than 20 and no search, one quiet line: "Search to find older files."
  - **Records**: the record types this cluster offers (`composableConcepts`): the
    marked ones, then the unmarked ones tagged "Unmarked" when Settings says to offer
    them. A record type is picked as a `query` source over its list query, as now.
  Each row is a `RecordRow`; pressing it puts the thing in or takes it out, and a
  picked row reads as selected (`aria-pressed`).
- **What is offered.** A file is offered only when the composer can read it: `.md`,
  `.markdown`, `.txt`, `.html`, `.htm`, `.css`, `.json`, `.csv`, `.svg`, `.png`,
  `.jpg`, `.jpeg`, `.gif` and `.zip` (the composer's own list in
  `integrations/compose/source_content.go`, which refuses the whole request for
  anything else). A record type with no list query is not offered (it was drawn
  disabled; a choice that cannot work is absent). Single rows (`concept_row`) are
  never picked here; they arrive only with a sender's setup, stand as cards like the
  rest, and can be removed.
- **States.** Each group shows `RecordListSkeleton` while it loads and a notice with
  the cluster's sentence when its read fails. A node that cannot read the record
  registry says "This node cannot read the record types, so none are offered."
  When both reads settle with nothing to offer: "Nothing to pick yet. Files you upload
  in Files appear here."

#### 2.6 Describe

A `TextArea` (four rows; accessible name "Describe what you want"; placeholder "A
one-page summary of the Q3 results for the board"). Optional. The stage's own title and
sentence say what it is for, so there is no visible field label and no caption.

#### 2.7 Name

An `Input` (accessible name "Name", placeholder "Q3 summary"), checked as it is typed
with the SDK's `fileNameProblem`, its problem said beside the field once the person has
typed (as now). The rail's answer is the filed name (the stem from `fileNameStem` plus
the kind's extension), so a person sees "Plan.md" before it exists. Enter makes the file
when the floor says Ready and moves on otherwise.

#### 2.8 Folder: the folder control

A `FolderSelect` (accessible name "Folder"): "Library (top level)" first, then the
person's live folders indented by depth, the shape Files' Move to folder already uses.
It is promoted to `clients/os/src/items/FolderSelect.tsx` on this second use and Files'
Move to folder draws it too. Choosing answers the step and moves on.

A folder a starting setup names is pre-selected and **sent as claimed**
(amendment 8): the app never swaps it for the top level. If the folder is not among the
person's live folders once that list has loaded, it stays selected as "Unavailable
folder", is still sent, and the cluster's refusal ("That folder isn't available. Choose
another folder.") is shown as worded; choosing another folder is then one click away
on the rail. While the folder list loads, the rail line shows its sentence rather than
a guessed name.

#### 2.9 Template and Organization

- **Template** appears only when at least one live template makes the chosen kind (its
  format is the kind's, or it names none): a step whose only answer is "No template" is
  furniture. `ChoiceStack`: "No template", then each template by name with its
  description.
- **Organization** appears for an email template (required: the server refuses an
  email template without exactly one active organization) and for a draft that already
  carries one, which only Start over of an older record makes. It uses the
  `AccountPicker`, required for an email template, with "No organization" otherwise.
  Ordinary files ask for no organization (decision D9).

#### 2.10 What is sent, and the three paths

`requestOf(draft)` builds the `MaterializeRequest` that `startMaterialize` sends, every
unanswered optional left out (sub-project 1's rule):

| Field | From |
|---|---|
| `name` | the name as typed, trimmed (the server drops one trailing extension of the kind) |
| `format` | the document's format; `html` for a package; `json` for an email template |
| `deployableKind` / `outputKind` | a package's kind / `email_template` |
| `templateId` | the template, when one is chosen |
| `folderId` | the folder, unless it is the top level |
| `accountIds` | `[organization]`, when one is set |
| `statement` | the description, when there is one |
| `sources` | the picked things as `{kind, ref, label}`; a Library file also carries `content: true` exactly when the path is *composed* |
| `requestId` | `useMaterialize`'s id for this ask (unchanged) |

The path decides what the floor says and what happens after the file exists:

- **blank**: no description, nothing picked, no template, a document kind. The server
  files an empty file with no model call. Detail "It starts blank."
- **described**: `isDescribedMarkdown(request)` (Markdown, a description, only Library
  files or nothing). The server files the empty page and this window hands the
  description to the Library's revision workflow (sub-project 1). Detail "You review its
  draft in the editor.", or, when a picked file is neither Markdown nor a picture,
  "Only Markdown files and pictures go with a description." (the hand-off leaves the
  others off and names them afterwards, amendment 7).
- **composed**: everything else. The composer writes it, reading the picked things.
  Detail "A model writes it."

`content: true` only on the composed path, because the server reads a `content`
reference before it decides the path (`captureSourceContents` runs before
`describedMarkdown()` in `integrations/compose/materialize.go`): a flag on a described
request would read, and could refuse, files nothing uses.

#### 2.11 Making: progress, Stop and Leave

- **The press starts the composition** (`useMaterialize`, over `startMaterialize`). The
  composition id in the reply puts the wizard into its making stage at once, before the
  composition's row reaches the feed, so a second press has nothing to press (the old
  form showed Materialize again in that gap).
- **The making stage is the same wizard**, as DESIGN.md's "A wizard can be come back
  to" requires: the steps keep their answers and no longer open; File is the open step,
  its mark moving, its body the file as it will be filed (glyph, filed name, folder).
  The bar: "Making the file", detail from the composition's status ("Starting" until
  the row arrives or while it is a draft, "Composing", "Writing the file"), tone busy,
  and a measure of the wait (m:ss since the run's `startedAt`, since the press until the
  run row arrives).
- **Stop** (text) asks the cluster to stop (`composeCancel`, reason "Stopped by the
  person"). Stopped before the file exists, the page reads "Stopped". If the cluster
  answers that it had already finished (`cancelled: false`), the file is kept
  (amendment 9): the page reads "Ready", nothing opens by itself, and a described file's
  description is not sent; its notice says "Stopped before the description was sent."
- **Leave** (the button) goes where Cancel goes. The making goes on: its row in
  Materialized shows the state, and opening the row returns to this stage.
- **When the file is ready**, the wizard gives way to the file's page, but only if the
  window is still showing this making stage; the app never moves a window the person
  has taken elsewhere. A failure or a dead run (`runState.ts`) goes to the page as
  "Failed" with its reason.

#### 2.12 What makes it playful, and where that stops

The things you mix are tangible: cards with their kind, their count and a remove
control, arriving under your hand. The rail is the recipe, ending in the file, whose
mark moves while the cluster works and which becomes the file's page when it exists.
Nothing else moves: no perpetual animation, no decorative chrome, motion only from
real events, and none under reduced motion (SUPERVISED-VISUAL-COMPOSITION.md,
"Supervision").

### 3. The finished file

#### 3.1 The page

`Materialized > <name>`, Back to Materialized. One `Panel` with `Facts`:

| Fact | Value |
|---|---|
| Kind of file | "Markdown", "Static site package", "Email template" |
| Filed in | "Library", the folder's name, "Unavailable folder" |
| Made from | the recorded sources' names, or "Nothing" |
| Description | the statement, or "None" |
| Template | the template's name (only when one was used) |
| Made by | the hollow or filled mark and the models ("No model", "anthropic/claude-sonnet-5 (2 calls)"), then the link "Open the run" (Nexus, when the person can open it) |
| Provenance | the record's `provenanceNote`, else the format's claim; absent while undecided |
| Organization | the organization's name (only when there is one) |
| Made | the moment, through the kit's `formatMoment` |

Under the facts, the sentence that says what the file is (sub-project 1's caption, in
the words the "Words on screen" table gives). Then, in place, the notices about the
description and references,
the editor's silence, and a refused tab reservation, all announced by the
always-mounted live region sub-project 1 built.

#### 3.2 Acts

| State | Acts, left to right (primary last) |
|---|---|
| Ready | Archive (text), Save as recipe (text, when a recipe could run and none was saved here), **Open in editor** |
| Ready, made from another app's setup | Back to <app> (text), Save as recipe (text) or else Archive (text), **Open in editor** |
| Ready, a package | Archive (text), Download (text), **Deploy it** |
| Ready, a package from another app's setup | Back to <app> (text), Download (text), **Deploy it** |
| Failed, Stopped | Archive (text), **Start over** |
| Archived | Open in editor (text, when it has a file), **Restore** |

The state word is the composition's ("Ready", "Failed", "Stopped", "Unknown") or
"Archived". A failure's detail is its reason, verbatim. An archived record's detail is
"The file is still in Files.", the consequence said where the act happened.

- **Open in editor** keeps Files' name for the act and its path
  (`openCheckedArtifact`: the browser editor, VS Code or Cursor per Files' setting; a
  ZIP downloads). "Download" is the package's name for the same act.
- **Back to <app>** is sub-project 1's way back: only on the file made from that setup,
  in a shell that holds the app, for a person who may open it. It ends when the person
  starts another file.
- **Save as recipe** saves as now (`recipeFacts`). Afterwards the act is gone from this
  page and a notice reads "Saved as a recipe. It is in Recipes."
- **Start over** opens Make a file filled in from the record: kind, template, the
  sources it records, description, name, folder and organization, with no way back to
  another app. It is a new ask with a new request id.
- **Deploy it** opens Deployables, as now (`handoff.ts`).

#### 3.3 Where the file opens

- **A file the cluster files blank opens by itself**: the blank and described paths.
  When the person's editor is the browser editor (Files' default), the Make the file
  press reserves the editor's tab (`browserHandoffPorts.reserve`, the reservation
  Campaigns' New template makes), and the file opens in it once its Files entry exists.
  A described file opens once its description has been handed on, so the editor opens on
  its draft being prepared. A failure or a Stop closes the reserved tab. A refused
  reservation is said on the page in the browser's terms, "Allow this app to open a new
  tab, then open the file again.", and Open in editor stays.
- **VS Code and Cursor open from Open in editor.** A browser launches another program
  only from a press, and the file exists seconds after the press.
- **A file a model writes opens from Open in editor.** It can take minutes, and a tab
  held open that long is worse than one press.

This is how #6035's "the result opens in VS Code" is met: the file always opens in the
editor, never in the OS, and by itself wherever the browser allows it.

#### 3.4 The description hand-off

Unchanged from sub-project 1 (amendments 5 to 7 and 11): this window hands a described
file's description to the Library once the composition is ready and its Files entry
exists, once per composition, under the request id the file was started with. One
addition: a file stopped after it was made is not handed on (section 2.11).

### 4. Materialized

- **Head** "Materialized"; meta: the count once the read settles. `Refine` (icon only):
  search over name and description, and the facet "Kind of file" as a kit `Select`
  ("Any kind", then the kinds). `AddButton` "Make a file".
- **Rows** (`RecordRow`): the kind's glyph; the name; the kind as the quiet second line;
  the provenance mark in the middle (`ProvenanceMark`: how much went in, hollow or
  filled for no model or a model); the state word, accent when ready; archived rows
  dimmed and listed only with Include archived; the live list's "new" tick on arrival
  only.
- **States.** `RecordListSkeleton` while loading. A failed read is a notice: "This
  cluster did not return what you made." with the cluster's sentence and Try again.
  Empty after a settled read: `EmptyState` "Nothing made yet" / "Make a file from your
  files, your records or a description." With archived ones hidden and nothing else:
  "Nothing made yet" / "Archived ones are hidden. Settings can include them." Nothing
  matching: "No matches. Clear the filters to see everything."

### 5. Templates

- **List**: Head "Templates", count, `Refine` (search), `AddButton` "Add a template".
  Rows: name, description as the second line, "Asks for <placeholders>" when it has
  any, the state "Active" or "Archived" (archived only with Include archived).
  `RecordListSkeleton` while loading; `EmptyState` "No templates yet" / "A template is a
  Library file whose look a new file can take."
- **Add a template** (`Wizard`; its orb wears a template glyph): **File** ("The Library
  file it is drawn from."; the From tray's file group, one choice, any Library file but
  a ZIP; with none: "Upload the file in Files first."), **Kind of file** ("What it makes."; pre-selected
  from the file's extension), **Name** ("What to call it."; starts as the file's name
  without its extension), **Description** ("What it is for."; optional, an `Input`),
  **Organization** ("Who it is for."; optional, only when the person has one). The
  forward act is "Add template"; a refusal reads "The template was not added." with the
  cluster's sentence. The template's page opens when it arrives.
- **Page**: `Templates > <name>`. Facts: Makes, File, Description, Organization, Asks
  for. Bar: Active with Archive (text); Archived with **Restore**.

### 6. Recipes

- **List**: Head "Recipes", count, `Refine` (search). No plus: a recipe is saved from a
  finished file. Rows: name, description as the second line, "Made 3 times, last on
  Oct 2" or "Not run yet" in the middle (rendered continuously, never an arrival), the
  kind as the state. `EmptyState` "No recipes yet" / "Save a finished file as a recipe
  to make it again."
- **Page**: `Recipes > <name>`. Facts: Kind of file, Made from (its selectors), Template,
  Filed in, Organization, Runs. Bar: Archive (text) and **Run it again**; Archived with
  **Restore**. Run it again opens the new composition's making stage (section 2.11).

### 7. Settings and readiness

- **Settings** is titled "Settings" (rule 1). The `SetupGroup` stays first. One group,
  "Preferences" (a `fieldset` whose legend dresses as a Subhead): **Opening tab** (a
  `Select` of Materialized, Make a file, Templates and Recipes, never Logs or Settings),
  **Kind of file to start on** (a `Select`), **Include archived** (a `Switch`) and
  **Offer every record type** (a `Switch`). Each preference is named once.
- **Readiness.** The app needs Storage and wants Inference (it needed both). A blank
  file needs no model, and New Markdown file must work on a cluster with no inference
  set up; what does need a model says so on the floor (section 2.3), so nothing is
  offered and then refused.

### 8. The starting setup

The contract in `clients/os/src/items/materialize.ts` is unchanged: `MATERIALIZER_APP_ID`,
`MATERIALIZER_COMPOSE_SECTION = "composer"`, `composeIntent`, `readComposeIntent` and the
keys `format`, `folderId`, `sources`, `outputKind`, `returnTo`. One addition:
`MATERIALIZER_MADE_SECTION = "materialized"`, which Files' "Open in Materializer" now
names, because a composition's page lives there.

- A setup opens Make a file with a fresh draft (`draftFromSetup`): its format answers
  Kind of file (`outputKind` makes it an email template); its folder answers Folder; its
  sources answer From; From and Describe count as passed; `returnTo` is kept for Cancel,
  Leave and Back to <app>.
- A setup lives and dies with its draft: Cancel, the trail's Back, a new setup, the
  plus and Start over discard it, and the first file made from it uses up its folder and
  its way back (sub-project 1's rule, restated for a wizard).
- `{compositionId}` (from Files) opens that composition: its page when finished, its
  making stage while it is still being made. Anything else is left standing, as now.

### 9. Data

- **One feed per concept at the app root.** Compositions, templates and recipes walk
  every page of their reads (fixing the stop at 50). Library files
  (`libraryArtifactsByKind({kind: "file"})`) and folders (`libraryFolders`) are retained
  at the root too, but only while a wizard or a page needs them, so opening the list
  alone does not seed the person's whole Library.
- **Runs of unfinished compositions** stay joined as now (`runState.ts`); the making
  stage also reads the run's `startedAt` for its measure.
- **The hand-off's wait for a Files entry** stays the narrow, per-file live read
  sub-project 1 built (`descriptionHandOff.ts`); the editor opening (section 3.3) uses
  the same wait.
- **Ids are bare**: an id is passed as the cluster gave it and compared with the SDK's
  `sameEntityId`; `idTail` is deleted.

### 10. What was removed, and why

- **The three-column composer** and its settled read-out: creation is the shell's one
  wizard; a record is a page.
- **The pending provenance chain** under the form: it claimed "by no model" before
  anything had run. The facts it carried are on the page, in `Facts` (rule 7); the
  row's provenance mark stays.
- **"Compose another"** in the Head: an act outside the bar (rule 12); the plus starts
  another file.
- **The Head's meta "a file, from rows in the graph"**, the captions "Open the finished
  file in your editor to review and make changes." and "Nothing picked yet. Everything
  above is something this cluster can read for you.", the template caption, the "Deploy
  it as" caption and the "Not offered yet" box: standing descriptions that help no
  decision (rule 7). The audio and video note stays in `docs/public/operate/memql-os.md`.
- **The required Organization** for ordinary files (D9).
- **The disabled record type** with no list query (a choice that cannot work is absent).
- **The inline Bind a file form** and the row buttons on templates and recipes (rules
  11 and 12).
- **"Materializer settings"**, the `Check` boxes, the spending note and the closing
  caption (rules 1, 4 and 7; switches for on/off preferences).

### 11. Files: New Markdown file

- The Add menu (`BrowseSection.tsx` `AddMenu`) reads, under "Into <folder>": **Upload
  files**, **New Markdown file**, **New folder**. New Markdown file is present only
  when the shell can open the Materializer for this person (`canOpen`); absent
  otherwise, never disabled.
- Choosing it calls `actions.openApp(MATERIALIZER_APP_ID, MATERIALIZER_COMPOSE_SECTION,
  composeIntent({ format: "markdown", folderId, returnTo: { appId: "files", sectionId:
  "browse" } }))` with the folder the menu names (`destinationFolderId`; left out when
  it is the Library top level: searching, the Bin and the Materializer place all name
  the top level already).
- The Add button's accessible name becomes "Add to this folder", which every item now
  does.
- "Open in Materializer" (row menu and inspector) names `MATERIALIZER_MADE_SECTION`.

The trip: Files, Add, New Markdown file; the Materializer opens on Name with Markdown
and the folder answered; a name and Make the file; the file opens in the browser
editor; the OS page offers Back to Files, where the new file is already in its folder.

## Words on screen

Every sentence is the contract; tests assert these strings.

| Where | Words |
|---|---|
| Wizard | title "Make a file"; rail "Making a file"; the step names and sentences in 2.1 |
| Rail answers | "Nothing picked", "Not described", "Library", "Unavailable folder", "No template", "No organization" |
| Kind of file | Markdown, Web page, Plain text, Spreadsheet (CSV), Data (JSON), Word document, PDF, Single-page app package, Static site package, Shopify storefront package ("A package for Deployables"), Email template ("Opens in the email editor") |
| From | "Search your files and records", "Search", "Files", "Records", "Unmarked", "Remove <name>", "Search to find older files.", "Nothing to pick yet. Files you upload in Files appear here.", "This node cannot read the record types, so none are offered." |
| Describe, Name, Folder | "Describe what you want", "A one-page summary of the Q3 results for the board", "Name", "Q3 summary", "Folder", "Library (top level)" |
| Floor | "Name the file"; "Describe it or pick what to make it from" + "A blank file can be Markdown, text, HTML, CSV or JSON." or "A template needs something to fill it."; "Choose an organization"; "Making this needs Inference" + "An owner or developer can set it up."; "Ready to make <name>" + "It starts blank." / "You review its draft in the editor." / "Only Markdown files and pictures go with a description." / "A model writes it."; "Making the file" + "Starting" / "Composing" / "Writing the file" |
| Acts | Cancel, Next, Make the file, Set up Inference, Stop, Leave; Open in editor, Back to <app>, Archive, Restore, Save as recipe, Download, Deploy it, Start over, Run it again, Add template |
| Refusals | "The file was not made.", "The template was not added.", each with the cluster's sentence |
| Page | facts in 3.1; "Nothing", "None", "Open the run"; "The file is still in Files."; "Saved as a recipe. It is in Recipes."; "Stopped before the description was sent." |
| Captions (sub-project 1, its open act renamed) | "This file was created blank."; "Your description was sent to be written. Open it in the editor to review and approve the draft."; "This file was created from a description. Open it in the editor to continue."; "The composed text is in the file." |
| Description notices (sub-project 1) | "The file was created, but the description was not sent for writing." + "Open it in the editor and choose Describe document to send it again."; "This reference wasn't attached." |
| Materialized | "Materialized", "Make a file", "Kind of file", "Any kind", "Nothing made yet", "Make a file from your files, your records or a description.", "Archived ones are hidden. Settings can include them.", "No matches. Clear the filters to see everything.", "This cluster did not return what you made." |
| Templates | "Templates", "Add a template", "No templates yet", "A template is a Library file whose look a new file can take.", "Upload the file in Files first.", "Active", "Archived", step sentences in section 5 |
| Recipes | "Recipes", "No recipes yet", "Save a finished file as a recipe to make it again.", "Not run yet", "Made 3 times, last on Oct 2" |
| Settings | "Settings", "Preferences", "Opening tab", "Kind of file to start on", "Include archived", "Offer every record type" |
| Files | "New Markdown file", "Add to this folder" |

## Failure handling

| Case | What the person sees | What remains |
|---|---|---|
| The start is refused (a folder that cannot be used, a reference the composer cannot read, a name it refuses) | "The file was not made." and the cluster's sentence over the steps; the draft as it was | Nothing written; the same press is the same ask |
| The reply to the start never arrives | The same notice with the transport's sentence; pressing again | The repeat finds the first composition, if one was made |
| The run fails, or its node is lost | The page: "Failed" and the reason | The composition `failed`; Start over |
| Stop before the file exists | The page: "Stopped"; the reserved tab closes | The composition `cancelled` |
| Stop races the file being made | The page: "Ready"; nothing opens; for a described file "Stopped before the description was sent." | The file in Files; the composition `ready` |
| The description cannot be handed on | The page's notice with the cluster's reason; the file opens anyway | The empty file and its composition with the statement |
| A reference is not attached | "This reference wasn't attached." with its name | The revision proceeds with the rest |
| The browser refuses the editor tab | "Allow this app to open a new tab, then open the file again." on the page | Open in editor |
| A desktop editor does not answer | The existing "The editor did not answer..." sentence on the page | Open in editor |
| Inference is not set up | "Making this needs Inference" on the floor, with Set up Inference for whoever can | A blank file can still be made |
| The window is closed while a file is being made | Nothing | The making goes on; a described file keeps its statement and the editor's empty page offers Describe document (sub-project 1) |

## Testing

All client tests run in `clients/os` with vitest; this design changes no Go and no DSL.
Generated files change: `component/memql/os_navigation.json` and
`component/architecture/embedded/platform.graph.json`.

- **The model** (`test/materializer/make.test.ts`, no DOM): steps per draft; the
  opening step for a plus, a setup and Start over; every floor row in 2.3 in order; the
  request for each path, including `content` only on the composed path and nothing sent
  for an unanswered optional; answers on the rail; filed names; readable references.
- **The wizard** (`wizard.test.tsx`, `making.test.tsx`): a setup opens on Name with
  Markdown and its folder answered; the plus opens on From; a choice moves on; Next and
  Enter; Make the file absent until legal and never disabled; a double press starts one
  composition with one request id; the making stage replaces the floor at once; Stop
  before and after the file; Leave; a reopened row shows the making stage; a refused
  folder is sent as claimed, kept as "Unavailable folder" and refused verbatim.
- **From** (`ingredients.test.tsx`): files and record types in one tray; search; pick
  and remove; unreadable files and record types with no list query absent; counts and
  problems on cards; loading, failure and nothing-to-pick states.
- **The page** (`page.test.tsx`, `pageActs.test.ts`): every row of the acts table; facts;
  captions; notices and the live region (sub-project 1's cases, moved here); Back to
  <app> rules; Save as recipe once; Start over fills the wizard; the run link only when
  Nexus can be opened.
- **Opening in the editor** (`editorOpening.test.tsx`): the browser editor's tab is
  reserved in the press and navigated once the Files entry exists (after the hand-off
  for a described file); closed on failure and Stop; never reserved for a composed file
  or a desktop editor; a refused reservation is said on the page.
- **Lists and sections** (`list.test.tsx`, `templates.test.tsx`, `recipes.test.tsx`,
  `settings.test.tsx`, `intent.test.tsx`): pages replace lists and come back to them;
  every page of a read is listed; counts after the read settles; the kit `Select` in
  Refine; Add a template; Run it again; Settings words and switches; the section
  contract (`composer` declared, `materialized` the made section); `{compositionId}`
  routing.
- **Files** (`test/files/newMarkdown.test.tsx`, `test/files/folderSelect.test.tsx`): New
  Markdown file sends the intent above with the folder the menu names, leaves the
  folder out at the top level, is absent when the Materializer cannot be opened; Move to
  folder still works through `FolderSelect`.
- **Guards**: sub-project 1's `onePathCreate` and `files/onePath` tests stay green; a
  corrections guard holds no disabled act, no raw `<select>`, no `Check` in Settings and
  no `idTail` under `apps/materializer`.
- **Rendered acceptance**: QA scenes for every surface (`clients/os/qa`), captured at
  1400 and 520 wide in light and dark, empty and populated, critiqued against
  DESIGN.md; then a live check on the local cluster, after the owner says go.

## Out of scope

- Email layouts in the Materializer: an email is written in VS Code's New email page
  (sub-projects 2 and 3). The `outputKind` key keeps working for a sender.
- Routing a described Markdown request with Library files the revision workflow cannot
  read to the composer instead (a server change in `describedMarkdown()` and the SDK's
  `isDescribedMarkdown`); see D31 and the owner list.
- A per-row record picker, uploading from the computer inside the wizard (uploads stay
  in Files, sub-project 1), page help content (the help modal still says documentation
  is coming), Ask record destinations for compositions, editing a template or recipe
  after it is made, and audio or video outputs.

## Decisions

| # | Decision | Reason |
|---|---|---|
| D1 | Making a file is the shell's one `Wizard`, "Make a file" | DESIGN.md "One wizard for adding" and Supervised Visual Composition ("guided workflows for setup and creation"); Files, Deployables and Fleet create this way; the rail doubles as a recipe a person can read back |
| D2 | Tabs are Materialized, Templates and Recipes; Make a file is the `composer` section with `parent: "materialized"` | Tabs are nouns; templates and recipes were two nouns in one scroller; the setup contract's section id is kept |
| D3 | The window opens on Materialized; the opening-tab preference can make it Make a file | Files, Deployables and Fleet open on their lists; a person who mostly makes says so once |
| D4 | Steps: Kind of file, Template (when one fits), From, Describe, Name, Folder, Organization (email template or carried), File | One question a step; a question waits until it makes sense; the order reads as one sentence |
| D5 | Every step but the name starts answered; the plus opens on From, a setup on Name, Start over on Describe | A blank file is a name and one press; the plus is for mixing; a sender has already said what kind and where |
| D6 | The floor offers Make the file the moment it is legal, from any step; otherwise Next; Cancel as text | One forward act, absent when not legal (rule 12); the rail is the review, so there is no Review step |
| D7 | The verbs are Make a file / Make the file / Making the file / "The file was not made." | Plain verbs; an act keeps its name through the flow; "Materialize" is jargon to a person |
| D8 | Kind of file is one choice: seven documents, three packages, and an email template only when a setup asks | The format-times-package selects offered meaningless pairs; email templates are written in VS Code's New email page |
| D9 | Ordinary files ask for no organization | Its tag only shared the Materializer's record (never the file) with that organization; VS Code New Markdown and Files uploads never ask; Files labels a file's clients in its inspector |
| D10 | Template appears only when a live template makes the kind | A question whose only answer is "No template" is furniture |
| D11 | From is one searchable tray of Library files and record types; picked things stand as cards | "Mix artifacts" as the owner put it; Library files were reachable only through a setup; cards make the mix tangible |
| D12 | Only files the composer can read are offered; record types with no list query are absent | A step knows before it offers; the composer refuses the whole request otherwise |
| D13 | `content: true` only on the composed path | The server reads a content reference before deciding the path, so a flag on a described request reads, and may refuse, files nothing uses |
| D14 | The Folder step shows the folder and lets the person change it; a claimed folder is sent as claimed and shown as "Unavailable folder" when unseen | The owner asked for the folder control; amendment 8 binds |
| D15 | `FolderSelect` is promoted to `items/` and used by Files' Move to folder too | Second use of the same control (DESIGN.md "Applying them") |
| D16 | After the press the same wizard shows the making stage, with Stop (text) and Leave (button) | "A wait is visible" and "a wizard can be come back to"; closes the double-press gap |
| D17 | A finished composition has its own page with Facts and one bar; "Open in editor" replaces "Open the file" | Rules 7, 11 and 12; one name per act across the shell |
| D18 | A file filed blank opens by itself in the browser editor reserved by the press; desktop editors and files a model writes open from Open in editor | #6035 asks that the result open in VS Code; a blank file exists in seconds and is written in the editor; a browser opens a tab or another program only from a press |
| D19 | Cancel and Leave go back to the app that sent the person, when there is one | The trip began elsewhere |
| D20 | Archive and Restore act on the record; the archived state says "The file is still in Files." | The consequence is said where the act happens, not in a standing caption |
| D21 | Templates: list, page and an Add a template wizard | The inline form above the list and the row buttons broke rules 11 and 12 |
| D22 | Recipes get their own tab and page, with Run it again in the bar | Two nouns, two tabs; acts in one bar |
| D23 | Settings is "Settings" with four preferences, switches for on/off | Rules 1, 4 and 7; Switch is the kit's binary preference |
| D24 | The app needs Storage and wants Inference; the floor says when a request needs Inference | A blank file needs no model, and New Markdown file must work on a cluster with no inference |
| D25 | Whole record types only; single rows only from a setup | Preserves today's capability without a per-row picker nobody asked for |
| D26 | Every read walks its pages; Library files and folders are read only while a wizard or page needs them | The count stopped at 50; the list alone should not seed the whole Library |
| D27 | Removed: the three columns, the pending chain, Compose another, the standing captions and boxes, the spending note | Rules 7, 11 and 12; each removal is listed in section 10 |
| D28 | `idTail` is deleted; ids are compared with `sameEntityId` | The bare-id contract |
| D29 | Files' Add menu: Upload files, New Markdown file, New folder; the button is "Add to this folder" | The owner's wording; files together, the folder last; the name says what every item does |
| D30 | New Markdown file is absent when the Materializer cannot be opened | An act that cannot work is absent, never disabled |
| D31 | A described Markdown file keeps sub-project 1's rule: only Markdown files and pictures go with the description, and the floor says so before the press | The revision workflow reads Markdown and images only (amendment 5); changing which path a request takes is a server change outside this OS-only design |

## Decisions for the owner to confirm

1. **Ordinary files no longer carry an organization** (D9). Only an email template
   asks for one; older records keep theirs.
2. **The file opens by itself only in the browser editor and only when it is filed
   blank** (D18). People who use VS Code or Cursor, and files a model writes, press Open
   in editor once.
3. **No email option in Make a file** (D8). This departs from the email program spec's
   section 6 ("Email shows the layout cards"): the email work routes Create new template
   to VS Code's New email page, so a second email front door here would duplicate it.
4. **The Materializer opens on Materialized**, not on Make a file (D3).
5. **A file other than Markdown or a picture (a CSV, say) mixed into a described
   Markdown file is left off the description** (D31). The floor says so; the
   alternative is a server change that sends such a request to the composer, which
   reads them.
