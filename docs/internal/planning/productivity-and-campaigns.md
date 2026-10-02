---
title: Productivity tools and organization marketing workflows
audience: internal
status: draft
area: planning
sinceVersion: 0.23.8
owner: znas
---

# Productivity tools and organization marketing workflows

Owner decisions recorded October 1, 2026. This is an implementation plan and
acceptance contract, not a statement that the work has shipped. It extends
[the email transport plan](email-transport-migration.md). Development and
testing are local; merging to main is authorized. Production deployment
remains subject to the owner's separate instruction.

## Two extensions, one cluster connection

Both extensions support desktop and browser-based VS Code on **macOS and
Linux**. Windows is outside the current support matrix. Ship and verify both
entry points; do not count a Node-only desktop build as browser support.
Native cluster installation stays desktop-only; browser instances can connect
to existing clusters. Supported desktop packages cover macOS/Linux x64 and
arm64 where the native dependencies support them. Document any narrower local
cluster installer hardware support separately from extension availability.

**MemQL** owns installing a local cluster, registering/selecting a cluster,
sign-in, connection lifecycle, language tooling, and cluster development and
operations. Installation requires a supported desktop host; browser VS Code
does not acquire permission or the ability to run local Docker simply by
loading the extension.

**MemQL Productivity Tools** is a separate extension and package, with the
same icon. It owns viewing and editing documents, email templates, and PDFs;
inline comments; suggested changes; and the user interface for approving
work arising from those suggestions. Useful file viewing and editing remain
available offline. Shared files, campaign records, collaborative reviews,
and execution require a MemQL connection.

Productivity Tools uses the existing extension's selected cluster and
authenticated client through a versioned API. It has no independent cluster
registry or credential store. The connection API exposes bounded operations
and connection changes, not access to raw stored refresh tokens. Requests
are authorized by the engine. Extension trust and hidden controls are not
authorization boundaries.

A document retains its source cluster, logical artifact identifier, and base
revision. Switching clusters or signing out cannot retarget an open buffer.
Pending responses from the former connection are discarded. Dirty content
must be preserved for the user to save locally or reconnect; it must not be
silently overwritten by a refresh. Saves compare the observed revision with
the current revision under a shared database lock and create a new revision.
Two editor windows on different replicas must not overwrite each other.

## Cockpit boundary and integration checks

The durable ownership contract is [the editor architecture](../../../editors/README.md#cockpit-owns-the-machine).
Cockpit remains the machine runtime and one-way backup agent. Neither extension
owns an additional file scanner, sync queue, spool, or backup ledger. Productivity
never runs work locally to bypass backend approval. A local file save can be
observed by the existing Cockpit watcher; a remote virtual-file save goes to
MemQL once and never writes back to the watched original.

Desktop registry changes must use one cross-process transaction contract, not
independent read/overwrite operations. Both clients preserve unknown keys.
VS Code authentication must remain in its own secret store and must not consume
Cockpit's refresh token. Test a stale editor registry snapshot against a Cockpit
update, and concurrent registrations in separate processes.

A watched file's stable backup identity must survive remote editor revisions
while each version reports its own honest upload provenance. Test an editor
save during a large Cockpit upload, through a different backend instance. Test
lost completion responses and subsequent retries after another edit: Cockpit
must receive the original upload's version, not the current head. Existing
version sessions without a recorded base must fail closed and be restarted.

## File opening and OS responsibilities

The **Files app stays**. There is no standalone Editor/File Editor app in
the current OS registry. Remove embedded file-content editors, including the
current Campaigns plain-text/HTML template editor, once their editor-side
replacements can serve the flow. Do not remove organization forms, names,
filters, scheduling controls, or other settings merely because they contain
a text field.

Opening an individual file defaults to VS Code in a **new browser tab**.
Settings can select desktop VS Code or Cursor. This choice is shared by
Files, desktop shortcuts, generated deliverables, and campaign templates.
URLs identify the cluster and file; they never contain bearer tokens,
refresh tokens, message contents, or secret configuration.

ZIP archives always download intact to the user's machine. No automatic
extraction, archive browsing, opening as a workspace, or executing contents
is part of this flow. Enforce this on Files list/context actions, inspectors,
desktop shortcuts, and the editor provider itself; a direct handoff link
must not bypass the rule. Detect by both stored MIME type and filename.

Microsoft's browser VS Code supports browser-compatible extensions and
virtual file providers. The existing Node-based MemQL extension cannot simply
be assumed to run there. Supply a web-compatible connection surface and
Productivity Tools bundle, and test the actual first-use installation,
sign-in, file open, save, and reconnect path. An empty vscode.dev tab is not
a successful handoff. Do not replace this with a custom embedded OS editor.

References: [Web extensions](https://code.visualstudio.com/api/extension-guides/web-extensions),
[virtual workspaces](https://code.visualstudio.com/api/extension-guides/virtual-workspaces),
[VS Code for the Web](https://code.visualstudio.com/docs/remote/vscode-web).

## Documents, PDFs, and review

MemQL is the durable backend for content, metadata, revisions, organization
authorization, comments, proposals, approval records, and jobs. The editor
is the interaction surface. Offline editing does not pretend to publish a
revision or create a collaborative comment; synchronization must be explicit
and preserve conflicts.

Markdown has three explicit modes: editable source, a rendered reading view
without Markdown syntax, and source beside rendered content. The reading view
supports selecting a passage and adding a comment or feedback thread. Source
and preview share the same document and unsaved edits; changing modes does
not discard the buffer or create another copy. Comments retain the saved
revision, selected quote, source range, and author. A later revision shows
an outdated anchor instead of silently relocating feedback. Both desktop and
browser hosts must test all three modes and selection in rendered content.
Offline viewing and editing work; shared comments require a connected MemQL
artifact, and unsaved content must be saved before a revision-bound comment.

PDF viewing and editing belong to Productivity Tools, including opening a
local PDF offline and saving a connected PDF as a new MemQL file revision.
Use a dedicated PDF custom editor; a plain text buffer is not PDF editing.
Document which modification tools ship (for example form fields, annotations,
page rotation, and page changes) and their limits. Do not claim arbitrary
text reflow or destructive redaction unless implemented and validated.

A comment is a stored review record, not an instruction that immediately
runs an agent. It anchors to an artifact revision and a text range/quote or
PDF page/location. Show stale anchors when the content has changed rather
than attaching them to a coincidentally identical line number.

Suggested changes retain the source revision, proposed change, author,
review state, and resulting work identifiers. A human approves a specific
proposal against a specific revision before MemQL starts a job applying
it. Changing the proposal or underlying revision invalidates that approval.
Store and check this on the server; hiding a button is insufficient.
Use MemQL's existing work and approval machinery rather than inventing an
extension-local task runner. Repeated approval requests must not start
duplicate jobs. A comment can remain a discussion without becoming work.

## Campaign authoring in the editor

Campaigns organizes audiences, sender connections, schedules, consent, and
results. Productivity Tools creates and edits template content and previews
merge fields. It can create a campaign template in a selected organization
and save a new revision of an existing template. A local template is not
automatically enabled for sending when saved to the cluster.

Readiness and sending approval remain explicit. Template edits must not
silently alter a run that is already sending. Test sends use the selected
organization's verified identity and remain distinct from campaign delivery
counters. The local Email app captures test messages without outbound mail.

### Create from examples and resources

The primary creation flow starts with **examples**, not a layout questionnaire.
A person supplies a reference PNG, a collection of files, or a ZIP of brand and
content resources, chooses the client organization, and briefly describes the
email they want. Optional labels distinguish a visual reference from an asset
to include (logo, product image, hero photograph). Materializer reads the actual
permitted source bytes and uses image understanding for visual references;
file names and metadata alone do not satisfy this requirement.

An explicit "Use as reference" operation may inspect supported members of a ZIP
inside the backend's bounded input-processing path. This is separate from opening
the archive in Files: ordinary ZIP opening still downloads it intact. Never
extract onto a user's machine or execute bundled scripts. Validate member paths,
entry counts, expanded sizes, MIME types, and image dimensions; report unsupported
resources rather than silently pretending to have used them. Do not recursively
expand nested archives or follow URLs found inside source material.

The same MemQL Materializer and Nexus pipeline owns the request, immutable source
snapshot, model routing, budgets, provenance, and resulting file. Productivity
Tools must not call a model vendor directly or start an independent worker. A
recipe records the brief and selected sources so the team can repeat it. A
regeneration creates a candidate for review; it never overwrites the approved
Campaigns template or sends a campaign.

The output is editable email HTML, a plain-text alternative, and a subject line,
not a screenshot of the email. Show source, rendered preview, and split modes in
Productivity Tools. Keep text, links, and images individually editable. A visual
reference provides design guidance; it must not cause the model to invent offers,
URLs, customer claims, or publish private assets. Preview uses supplied example
merge-field values and safely isolates generated HTML. Publishing explicitly
checks the client's organization and creates a Campaigns template revision;
sending and scheduling remain Campaigns responsibilities.

The implemented inclusion contract uses `includeImages` on an explicit
`library_file` content source and preserves it in recipes and execution inputs.
Only included sources receive model-visible asset handles. Storage URLs are
removed from the model's captured-file context. Handles resolve to the captured
image bytes; an unselected, changed, or invented image is refused. The draft
stores bounded raster data in HTML, and transports convert it to CID attachments.
This preserves the existing immutable template and scheduled-send snapshots
without exposing an image bucket. Limits: eight included images, 1 MiB combined
image data, 2 MiB for the whole template. No automatic image optimization is
currently performed. ACS inline attachments are a provider preview feature;
SMTP uses standard MIME and the local test inbox makes no external delivery.

## A client's storefront subscription journey

Each deployable belongs to an organization. A storefront's subscription form
is bound by the server to that organization, its store, and a configured
marketing audience. The visitor cannot choose a different account, audience,
sender identity, or owner in form parameters.

An **audience** is the marketing recipient list. It is not an identity group
that grants access to MemQL. Subscribing never creates an administrative
membership or gives a shopper application permissions.

Record the address, normalization, source storefront, consent text/version,
and the subscription state. Repeated form submissions are idempotent. When
confirmation is required, an unconfirmed address receives confirmation only
and is not yet eligible for marketing. Completing the subscription can
trigger one welcome/thank-you email through that organization's transport
and template. Repeated delivery of the same subscription event must not send
multiple welcome messages. An unsubscribe is respected at the point of send.

The same organization can own multiple independent campaigns. Support one-time
sends and recurring cadences, including every two or three weeks. Each
occurrence has its own durable send job and per-recipient ledger. Pausing a
series prevents future occurrences; resuming must not replay every missed
occurrence in a burst. Make the next send time and time zone visible.
Changes to a series affect future occurrences rather than rewriting history.

Consent withdrawal is scoped to the client organization represented by the
message. Keep a separately governed cluster-wide block for delivery safety
(for example confirmed hard bounces). Preserve existing suppression and
unsubscribe links during the transition; absence of an organization on an
old record must not silently unsuppress an address.

## Acceptance checks

- An operator managing two clients must select the organization and cannot
  send using the other client's template, audience, connection, or domain.
- A storefront subscription reaches the correct audience and sends exactly
  one welcome message; duplicate submissions and replica changes do not
  duplicate it. Missing consent or disabled signup prevents enrollment.
- Two- and three-week schedules produce distinct send histories, remain
  isolated per client, and respect pause, cancellation, and unsubscribe.
- Files and desktop shortcuts use the browser editor by default; installed
  VS Code/Cursor preferences work; ZIPs download without extraction.
- Browser and desktop editors share the active cluster through the MemQL
  extension. Offline editing remains usable without silently claiming sync.
- Text, templates, and PDFs save new revisions; a concurrent save on another
  replica reports a conflict instead of overwriting content.
- Comments persist in MemQL and appear in another editor session. Applying
  a suggestion requires human approval of unchanged content and records a
  single durable work run.
- No embedded OS file-content editor, duplicate credential store, or new
  email SaaS vendor remains in the delivered workflow.


## Implemented Markdown revision review

Productivity's reading view can select current comments and prepare a captured
revision proposal. The proposal and source bytes live in the existing private
Nexus goal input and approval subject, with stable goal/run/approval identities
and a request fingerprint. A request never publishes a running job before its
human decision. The server rechecks the current readable backing source, exact
revision, bytes and write authority at approval and immediately before execution.
A comment does not grant access to its document. Review work identities reject
raw client insert/update calls, and a running job cannot approve itself.

The fixed `reviseLibraryDocument` template uses the existing Materializer,
model routing and a one-call ceiling. It writes a separate Markdown draft;
Productivity compares captured source and draft in read-only editors. Applying
that reviewed content uses the original versioned save contract. Request retries
recover partial bootstrap; decision retries recover the fixed-template resume
without replaying unrelated approval effects. No worker, model connection,
credential store, publication action or email send is added to the extension.
Limits are 20 selected comments, 64 KiB combined feedback, an 8 KiB instruction
and a 128 KiB UTF-8 Markdown source. This does not yet implement reply threads or
feedback-driven PDF/email-template revision. The latter formats retain their
existing editing, preview and explicit publication flows.
