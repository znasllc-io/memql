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

Discussion replies and threaded resolution are not available yet. Comments
and explicitly approved Markdown revision jobs are supported.

For a checkout, build the SDK first, then `npm ci` and `npm run compile` in
this directory. `npm test` exercises pure logic. The host suite uses
`node test/runner.cjs` for web or `node test/runner.cjs --desktop` for desktop,
after both extensions have been compiled. `npm run package` creates a VSIX
with both entry points; it does not publish it.

Markdown has **Source**, **Read**, and **Review** modes. Read is a clean reading
surface. Review offers one feedback action for selections and sections, plus
a document extension entry point at the end. **Markdown Split View** remains
available in the editor title and command palette. These views share one VS Code
document, including unsaved changes. Save before adding shared feedback. Local files remain local; opening
one never silently uploads it or starts another Cockpit backup process.

Review each proposal with **Accept**, **Decline**, or **Modify with AI**, then
choose **Apply accepted**. Linked edits, such as moving a passage, share one
decision. Modifying an item preserves the other proposed items and their
decisions. The DSL workflow uses the harness for evidence collection when the
feedback calls for research, then prepares exact replacements for human review.
Unchanged text and formatting are preserved by default. **Show in document**
keeps the target highlighted; expanded explanations survive status updates.

In the MemQL browser editor, **Dictate** in feedback, extension and per-item
modification composers uses Ask's
microphone capture and authenticated transcription stream. Browser microphone
permission is requested on first use. Stop to finish transcribing, edit the
transcript, then add it to review. Nothing is submitted automatically. Closing
the view, switching to Read or changing the connection cancels capture. Ordinary desktop VS Code
does not expose this capture adapter, so its feedback composer remains text-only.
The cluster must offer an eligible speech recognition model: a running Whisper
endpoint also needs its Cockpit worker registered, connected and allowed to
serve that model. Text-model readiness alone does not establish dictation readiness.

The immutable approval records human feedback separately from proposed text,
the measured provider/model calls behind each item, the exact accepted subset,
and the source revision. A modified item gets its new attribution while retained
items keep theirs. Existing document/file version history remains the source of
earlier authorship; this does not infer missing model identities for imported
or historical content.

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

In **Review**, select a passage and choose **Feedback**, use the feedback icon
beside a heading, or press Ctrl/Cmd+Alt+M. Describe what you want; the DSL-driven
harness infers additions, revisions, deletion, moves and research from that
request. Selecting a section supplies context without forcing an operation.
**Extend document** at the end also starts a request without a selection. Unfinished notes stay attached to their selection across view
changes; if the source changes, their text is preserved and must be reselected.
Both composers use **Add to review**. The **Requests** list uses labeled
**Included / Excluded** switches to choose what enters the next proposal; a
previous completed review is collapsed. Submitted requests become read-only
and appear beside their corresponding changes, without inclusion controls.
**Propose changes** generates the same before/after review and
approval controls for passage feedback and standalone extensions. Overlapping
status reads cannot let an old completed request hide a new proposal.
Reopening an editor restores the latest owned request from a DSL lookup in
MemQL, even when the browser has an older saved receipt. Request ordering uses
admission time, so later heartbeats on an older review cannot displace it.

Choose up to 100 current notes and **Propose changes**. This starts a bounded
AI revision against the exact saved Markdown source, up to 128 KiB. The DSL
`reviseLibraryDocument` template orders source capture, harness evidence gathering,
the named revision prompt, validation, human review and application. The native capabilities enforce
ownership, source revisions, unambiguous non-overlapping replacements, exact
approval and versioned storage. No generation-specific metadata is required:
imported Markdown uses the same source, quote, context and position anchors.

The review panel shows the original and proposed text for every affected
location, including both ends of a move. Location links scroll to the affected
passage without closing Review; proposal links use the actual insertion location,
even when the request started at the document end. At narrow widths Review
becomes a bottom panel, keeping the document reachable above it. **Compare full document** opens a
read-only diff. **Apply accepted** resumes the same Nexus job and saves a new
version containing only accepted items; declined items are excluded. Preservation is the default:
people can say "Rename this to Draft Exchange" without asking to retain bold
formatting, surrounding descriptions or other items. The DSL prompt requires
minimal changes and a check that every difference serves the feedback. Native
application copies unaffected bytes and matching context from the saved source.
Explicit document-end extension requests also enforce insertion-only changes;
addition-only feedback follows the preservation contract in the DSL prompt. Additions default to
the selected section or passage, or append when started at the document end.
Instructions can direct an addition elsewhere, such as question time after lunch
in a schedule. The button's location does not constrain the insertion point.
Requests can also delete, rearrange or extend content. The
panel follows progress and refreshes a clean editor after application, while
preserving unsaved local edits. Invalid or ambiguous AI output fails without a
write; a failed or declined attempt can be submitted again.

Opening a document and saving notes do not start AI work. Approval is tied to
the exact proposal and source revision; intervening edits or revoked authority
refuse the save. Repeated submissions and decisions recover existing receipts,
and resumption on another replica reuses the journaled AI result. Feedback-driven
revision currently supports Markdown; email composition uses separate controls.
