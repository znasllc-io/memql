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

This package is under development. Campaign publishing, discussion replies, and
human approval workflows are tracked in the repository's productivity plan;
this README does not claim those unfinished capabilities are available.

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
Images are design references; private assets are not automatically published to
public email URLs. A missing approved image URL remains an editable placeholder.

Email **Source**, **Preview**, and **Split** use the same document, including
unsaved changes. Preview preserves presentation HTML, blocks active content and
external loads, and shows the plain-text alternative. AI creation is connected;
ordinary template source editing and preview work offline. Creating the draft
does not publish it to Campaigns or send mail.
