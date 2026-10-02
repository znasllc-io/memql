# MemQL Productivity Tools

Documents, email templates, and PDFs in Visual Studio Code and Cursor, using
the same MemQL icon and the connection provided by the **MemQL** extension.
Supported hosts are desktop and VS Code for the Web on macOS and Linux.
Windows is not supported.

Install **MemQL** and **MemQL Productivity Tools** together. The MemQL extension
owns cluster selection, sign-in, credentials, language tools, and native cluster
installation. Productivity Tools adds file work; it does not install clusters
or keep another set of credentials.

- Offline: read Markdown, edit its source, or split source and rendered content; compose and preview `*.email.json` templates,
  view PDFs, rotate pages, and add text. PDF editing supports undo, redo, Save,
  Save As, and recovery backups. It does not replace arbitrary existing PDF text.
- Connected: open individual `memql-file:` resources from MemQL Files and save
  new revisions, and add comments to selected Markdown passages. Feedback is stored
  against the saved revision; older anchors stay marked as earlier revisions. Concurrent changes are refused instead of overwritten. A
  change of cluster or connection invalidates the previous document's save.
- Opening ZIP archives downloads them intact. They cannot be edited or opened
  as workspaces. Explicitly selecting one as a Materializer reference lets the
  backend inspect supported resource members in memory.

Files larger than 32 MiB should be downloaded through MemQL Files. Browser
file access follows VS Code's file-picker permissions. PDF content is rendered
from supplied bytes with local fonts and workers; the viewer has no remote
document loading or script execution from the PDF.

This package is under development. Discussion replies and
human approval workflows are tracked in the repository's productivity plan;
those unfinished capabilities are not available.

For a checkout, build the SDK first, then `npm ci` and `npm run compile` in
this directory. `npm test` exercises pure logic. The host suite uses
`node test/runner.cjs` for web or `node test/runner.cjs --desktop` for desktop,
after both extensions have been compiled. `npm run package` creates a VSIX
with both entry points; it does not publish it.

Markdown mode controls are available above the reading view and from the command
palette: **Markdown Source**, **Markdown Reading View**, and **Markdown Split
View**. The source and preview share one VS Code document, including unsaved
changes. Save before adding shared feedback. Local files remain local; opening
one never silently uploads it or starts another Cockpit backup process.

The reading view uses an HTML-disabled Markdown renderer and does not fetch
remote images. Links open only after a deliberate click. Shared comments require
a connected MemQL file; local review drafts are not presented as synchronized.
Before accepting a new comment, the cluster verifies the selected source lines
against the saved UTF-8 document (up to 2 MiB), under the same version lock as
saves. Stale or fabricated passages are refused. Retrying a saved comment after
a lost response returns its receipt, even if the document has since changed.
The current comment list shows the newest 500 records and reports when more are
stored. Applying feedback automatically is not enabled by adding a comment.

**Create Email from Examples** asks for an organization, reference files, a name,
and a brief. Select a PNG, JPEG, GIF, text/HTML/CSS/Markdown/JSON/CSV/SVG file,
or ZIP bundle from the device or recent MemQL Files. The cluster captures the
permitted source bytes before dispatch, analyzes image references through its
own model router, and creates an editable `*.email.json` draft. The work appears
in Materializer and Nexus. Choose **Save as recipe and create draft** to reuse
the brief and references; **Open Last Email Composition** resumes viewing after
an editor reload. Cancelling the progress notification requests cancellation
through MemQL. Uploaded references remain in Files even if a later step is cancelled.

References are bounded to 64 files, eight images, 16 MiB expanded content, and
256 KiB of text per composition; each image must be at most 24 million pixels.
ZIP processing rejects unsafe paths, symlinks, nested archives, and unsupported
files. PDF and Word reference extraction are not offered by this flow yet.
Images are references by default. **Images to include** separately selects logos,
photos, or the images within a ZIP for the actual email. Included images must be
PNG, JPEG, or GIF, with up to eight images totaling 1 MiB. Large layout examples
can stay reference-only. The backend checks this limit before invoking a model.
The editable draft carries selected image bytes; SMTP/Graph sends encode them as
inline MIME attachments; ACS uses its CID attachment field. Live Azure setup and
recipient-client rendering verification remain separate checks. No public image hosting or private Library URL is required. The local Email app can display these
embedded assets while continuing to block remote tracking images. Each template
file remains limited to 2 MiB, including its image content. Generated drafts use
inline CSS and selected `img` assets; external CSS, alternate image sources and
conditional markup are refused instead of bypassing the inclusion choices.

Email **Source**, **Preview**, and **Split** use the same document, including
unsaved changes. Preview preserves presentation HTML, blocks active content and
external loads, and shows the plain-text alternative. Expand **Sample values**
to preview personalization fields used by the source; these values never change
the stored template or enroll a recipient. AI creation is connected;
ordinary template source editing and preview work offline. Creating the draft
does not publish it to Campaigns or send mail.

**Use in Campaigns** saves the current email as a draft or publishes the exact
reviewed revision for the selected client organization. Publishing makes the
template selectable; it never sends a message. Reopening a template from
Campaigns gives a `memql-file:` document whose saves retain its organization and
check the loaded revision. A content edit returns the template to draft. Compare
with the latest revision to reconcile another person's changes before saving.
Plain-text templates remain supported with an empty `htmlBody`.

Campaigns keeps names, organization selection, lifecycle, schedules, and results.
Its embedded email-body editor is removed. Browser VS Code remains the default;
the Files editor preference can select installed VS Code or Cursor. A queued send
captures the ready template content it reviewed, so later copy edits do not
change an in-progress campaign. Existing jobs without a captured snapshot retain
the older live-template behavior; pause and recreate those jobs to capture a copy.
