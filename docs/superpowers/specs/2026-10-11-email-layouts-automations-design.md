# Email layouts, one Email app, and automations

- **Date:** 2026-10-11
- **Status:** owner direction of 2026-10-11 ("the brainstorming is done, execute"); decisions below are the implementer's rulings on the points the brainstorm left open
- **Program:** epic #6022, sub-projects 2 to 5 (#6032, #6033, #6034, #6035) plus #6036/#6037 follow-ups, building on sub-project 1 (`2026-10-11-materializer-front-door-design.md`)
- **Owner approvals recorded 2026-10-11:** three new public email-link pages (confirm subscription, view in browser, manage preferences) beside `/unsubscribe` and `/t/`

## In plain terms

- **Layouts.** An email template is a structured document: a layout (a column of blocks: logo, banner, tinted body, button, signature, social links, footer) plus a theme (brand colours and fonts). Four built-in layouts come from the eight production emails the owner supplied for a client (a two-email welcome and a six-email series): **Confirmation**, **Letter**, **Article** and **Spotlight**. An organization saves its own finished email as a layout, keeping its logo, footer and signature, so the next email in the series comes out identical apart from the words and pictures. Given the same resources the renderer reproduces the originals; a screenshot comparison against the owner's examples is the acceptance check.
- **VS Code is where an email is written.** "New email" opens a page in the productivity tool: choose the organization and a layout, describe the email, and the Materializer writes it into the layout; or start from the layout and write it yourself. The editor shows the rendered email beside an outline of its blocks; selecting a block edits its words, link or picture. Publishing makes it a template in the Email app.
- **One Email app.** Campaigns moves into Email. Its tabs are nouns: Overview, Campaigns, Automations, Audiences, Templates (and Inbox for owners and developers on a capture cluster). Adding anything runs the kit `Wizard`; every record has one page with one `ActionBar`. Setup (sending domains, senders, the testing audience) lives in the app's Settings. The `MEMQL_EMAIL_APP_ENABLED` switch that hid Email in production is removed.
- **Automations.** An automation is a trigger followed by steps, drawn as a vertical flow: "When someone joins Newsletter" -> "Send Welcome 1 and wait for them to confirm (7 days)" -> "Send Welcome 2" -> "Wait 3 days" -> "Send Stage B1" ... Build it by hand in the flow, or describe it in a sentence and review the drafted flow before turning it on (Fleet's "Describe it instead" pattern). Double opt-in is real: a confirmation email carries a confirm link, and unconfirmed addresses never receive marketing.
- **The coworker's work is kept and corrected.** Every capability that exists stays reachable. The audit's defects (a custom stepper instead of the Wizard, state-changing acts scattered over seven panels, disabled acts, loading drawn as warnings, node-local rule arming, the 100-recipient truncation, the footer appended after `</html>`, a collapsing delivery ledger, and others listed in "Corrections") are fixed.

## Decisions

| Question | Ruling | Why |
|---|---|---|
| Where layouts live | Built-in layouts are embedded in the engine; organization layouts are `v1:campaigns:emailLayout` rows | Built-ins version with the renderer; org layouts are data an organization owns |
| What the AI writes | The CONTENT of a layout's content blocks (by block id), plus subject and preheader. Brand blocks and structure come from the layout untouched | "Exactly like that" means structure, spacing and brand are fixed; only words and chosen pictures change |
| Template file format | `*.email.json` keeps its name. Layout mode: `{subject, preheader, layout, theme, blocks}`. Custom mode (pasted or hand-written HTML): `{subject, preheader?, textBody, htmlBody}` | Two real authoring modes; existing custom templates stay valid without a converter |
| Images in a document | `artifact:<artifactId>` references to Library files (or `data:` rasters); resolved and embedded at render and publish | Editable source, reusable brand assets; published snapshots stay self-contained (CID at send, as today) |
| Where rendering runs | One Go renderer (`component/compose/emaildoc`), exposed to editors as the `emailRender` builtin | One renderer means the preview is the email that is sent |
| Creating a template from the OS | "Create new template" in the campaign wizard opens the VS Code "New email" page with the organization chosen; the wizard waits for the published template and selects it | The owner's words: it opens the productivity tool, like opening a `.md` file |
| Event triggers | Existing rules (event -> one email) stay, are listed and drawn in Automations, and are fixed (cross-node arming, pagination). Journeys add audience triggers with multi-step sequences | Keeps every shipped capability; the welcome-and-series emails need audience triggers and waits, which rules never had |
| Newsletter welcome | The binding's welcome template becomes optional; an automation on the audience is the recommended welcome | Avoids two welcome mechanisms sending twice |
| Confirm link safety | GET shows a page with a Confirm button; POST confirms | Link scanners prefetch GETs |

## 1. The email document

### 1.1 Format (`*.email.json`, parsed by `emaildoc.Parse`)

```jsonc
{
  "subject": "Three ways to get more from your plan",   // required, one line, < 999 bytes
  "preheader": "A short guide for your first week",      // optional, <= 200 chars
  "layout": "builtin:article",                        // or "org:<layoutId>"; provenance only
  "theme": { "page": "#eef0f3", "canvas": "#ffffff", "primary": "#2f5d8a", "secondary": "#6f93b8",
             "accent": "#d9822b", "tint": "#f5f4f0", "text": "#2b2b2b", "muted": "#8a8a8a",
             "headingFont": "Georgia", "bodyFont": "Helvetica" },
  "blocks": [ /* see 1.2 */ ]
}
```

Exactly one mode: `blocks` (layout mode) or `htmlBody` + `textBody` (custom mode). Unknown keys are refused. 2 MiB cap. Fonts render as `<name>, Arial, Helvetica, sans-serif` with no web-font link unless `theme.webFonts` is true (the examples carry none).

### 1.2 Blocks

Every block has `id` (stable, unique in the document), `type`, and optional `role` (`"brand"` or `"content"`, default content). Colours name theme tokens: `page canvas primary secondary accent tint text muted white`.

| type | fields | used for |
|---|---|---|
| `image` | `src` (`artifact:<id>` or `data:image/png|jpeg|gif`), `alt`, `width` (px <= 600), `align`, `href?`, `bleed?` (full width, no side padding) | logo, banner, mascot, photo |
| `heading` | `text`, `size` (px), `color`, `align`, `italic?`, `weight?`, `font` (`heading`/`body`) | a welcome headline, a two-tone question prompt |
| `text` | `text` (Markdown subset: paragraphs, `**bold**`, `*italic*`, `- ` and `1. ` lists, links, merge tags), `size`, `lineHeight`, `color`, `align`, `font` | letter and article bodies |
| `highlight` | `text` | the accent band statement |
| `button` | `label`, `href`, `color`, `width` | "Confirm my email", "Ask a question" |
| `divider` | `color` | hairlines |
| `spacer` | `height` | |
| `section` | `background`, `blocks` (no nested sections) | tinted body, brand-colour hero, signature band |
| `columns` | `columns: [{width, blocks}, {width, blocks}]`, `background?` | signature (photo, name, title), promo (title and text, mascot) |
| `social` | `title?`, `links: [{network, href, icon?}]` | "Follow us on" |
| `footer` | `links: [{label, href}]`, `lines: [string]` | the compliance footer |
| `html` | `html` (sanitized) | escape hatch inside a layout |

Merge tags anywhere text appears: `{{displayName}} {{email}} {{accountName}} {{fields.<key>}}` (existing) and the link tags `{{unsubscribeUrl}} {{preferencesUrl}} {{viewUrl}} {{confirmUrl}}` (new, section 4.4).

### 1.3 Rendering (`component/compose/emaildoc`)

- `Render(doc, resolver) -> {HTML, Text, Issues}`. Table layout, inline styles, 600px canvas with the examples' mobile rule (`.ecw` full width below 599px), Outlook conditionals, hidden preheader. Spacing and sizes come from the blocks, so a layout reproduces its source email.
- `Text` is generated: headings, paragraphs, list items, `Label: URL` for buttons, the footer.
- `Issues` (never fatal for preview, fatal for publish): an image with no `src`, a `confirm` step template without `{{confirmUrl}}`, a missing unsubscribe link in a marketing template (section 5, F11), more than 8 images or 1 MiB of image data.
- A preview render marks blocks with `data-block="<id>"` so the editor can select by clicking; a send render never does.

### 1.4 Layouts

- **Built-in** (Go-embedded documents, neutral example brand and copy): `builtin:confirmation`, `builtin:letter`, `builtin:article`, `builtin:spotlight`. Their structure, sizes and spacing are taken from the owner's examples. They contain none of the client's copy, pictures or brand: this repository is public; the client reproduction lives in that organization's own saved layouts and in a local fidelity harness (section 7).
- **Organization layouts:** `v1:campaigns:emailLayout {ownerUserId!, accountId!, name!, description, document object!, sourceArtifactId, status (active|archived)}`; `@rowAuthz(owner, clusterOwner, account)`. Saved from a finished email with "Save as layout": the whole document, with each block's role. Brand blocks keep their content; content blocks keep theirs as the example the AI writes against.
- `emailLayouts({accountId})` lists built-ins then the organization's active layouts. `campaignSaveLayout({layoutId, accountId, name, description, document, expectedRevision})` validates with `emaildoc.Parse` and saves; archive and restore follow the template pattern.

### 1.5 Materializer email path

- `composeMaterialize` gains `layoutId`. `outputKind: "email_template"` without a layout uses `builtin:article`.
- **Blank email** (layout, no statement, no sources): the layout document is filed as the new `.email.json`, with no model call (the email analogue of sub-project 1's blank Markdown).
- **Described email:** the composer receives the layout's blocks (type, role, current text as the example, sizes), the statement and the references (vision as today). Its strict schema is `{subject, preheader, content: [{id, text?, label?, href?, alt?, image?}]}` where `image` names an included asset handle. The server merges content into content blocks by id, keeps brand blocks, resolves handles to `artifact:` references, validates with `emaildoc.Parse`, and files the document. A content id the layout lacks, a brand id, or an invented image is refused (as invented images are today).

### 1.6 Preview and publish

- `emailRender({document, accountId, sample?})` -> `{html, text, issues}`: resolves `artifact:` images the caller can read, substitutes `sample` merge values, returns the preview render. Editors and the OS template page use it.
- `campaignSaveTemplate.content` accepts the document in either mode. The server renders it, stores `subject`, `textBody`, `htmlBody` as today plus `document` (object), `layoutId`, `preheader`, and `sourceArtifactId` (the Library file it was published from). Publishing a file whose `sourceArtifactId` already has a template in that organization updates that template; the browser-local mapping in `workspaceState` goes away.

## 2. VS Code: writing an email

Follows the extension UX record (`docs/internal/design/2026-09-28-vscode-extension-ux.md`) and the Markdown editor's patterns: quiet page, one action bar, state in words, at most three acts with the primary last, nothing typed is lost, loading drawn as the shape of the email.

### 2.1 New email (`memql.productivity.newEmail`; replaces `newTemplate` and `templateFromExamples`)

A webview page, "New email":
1. **Organization** (skipped when the person has one).
2. **Layout**: built-in and organization layouts as cards, each a scaled render of the layout. One choice.
3. **Name.**
4. **Describe it** (optional): a text box ("What should this email say? Include the audience, the offer and any links."), with references (images, brand files, a ZIP) and pictures to include, as the examples flow allows today, and dictation where the host has it.

Action bar: state ("Choose a layout", "Ready to write", "Writing your email", "Couldn't write the email") and acts: **Start from the layout** (text act; blank email, no model) and **Write it** (primary; present once there is a description). Both create through the SDK `materialize` helper (`outputKind: "email_template"`, `layoutId`, `accountIds`, a `requestId` per click). While the Materializer works the page shows the skeleton of the chosen layout; Cancel stops it. The result opens in the email editor.

`memql-file://<domain>/new-email/<bareAccountId>/New%20email` opens this page from the OS: a `new-email` resource kind with the organization as its id, so it fits the existing three-segment, query-free resource grammar that all three validators enforce (`documents.ts`, `editors/browser/src/handoff.mjs`, `clients/os/src/items/editorPreference.ts`). The editor resolves the email's organization before its first preview (from the template it was published as, its composition, or a single membership; otherwise it asks once on the page), so Publish never prompts.

### 2.2 The email editor (`*.email.json` custom editor)

- **Design** (default): the rendered email at a desktop width with a Desktop/Mobile width toggle, beside an inspector. The inspector holds the email's subject and preheader, the outline of blocks (select, move up or down, add after, remove), and the selected block's fields (words, link, button label, picture: choose from Library, upload, or remove; colours from the theme). Clicking a block in the preview selects it. Theme colours and fonts sit in a "Theme" disclosure. Sample merge values sit in a "Preview with" disclosure and persist with the page state.
- **Source**: the JSON in a text editor (the same buffer).
- **Custom mode** documents open with the rendered HTML and the Source editor (no outline).
- **Action bar**: state in words ("Draft", "Published to <organization>", "Unsaved changes", "Changes since publishing") and acts: **Save as layout** (text), **Publish** (primary). Publish calls `campaignSaveTemplate` with `action: publish` for the organization the editor already resolved. The editor never opens a second tab.
- Images go in through the existing upload path (`Documents.createFile`, allowed by sub-project 1's guard as an upload of bytes the person has).

## 3. The Email app

Campaigns' manifest, sections and code move into `clients/os/src/apps/email/`; the `campaigns` app id is removed (pre-release: no alias). Resources: `app:email` (all roles), `app:email/inbox` (owner, developer), `app:email/logs` (owner, developer, admin). Every `app:campaigns*` seed, mirror and reference moves to `app:email*`.

### 3.1 Sections

| Tab | List row | Add (Wizard) | Record page acts (ActionBar) |
|---|---|---|---|
| Overview | `kit/Overview`: sent (30 days), open rate, click rate, subscribers, active automations; absent is not zero | | |
| Campaigns | name, audience, state, sent/scheduled time | Add a campaign | per state, below |
| Automations | name, trigger sentence, state, enrolled | Add an automation (+ Describe it instead) | Turn on / Pause / Retire |
| Audiences | name, subscribers, confirmation, signup form | Add an audience | Archive / Restore |
| Templates | name, layout, state, organization | Create new template (opens VS Code) | Open in editor / Archive / Restore |
| Inbox | owner/developer, capture clusters only | | |

Settings (header gear), titled **Settings**: *Sending* (organization, sending domains with the Azure wizard, senders), *Testing* (testing audience), *Preferences* (Switches: include archived, track opens and clicks by default; Select: opening tab), *Package* (import/export). Logs (header icon) keeps `AppLogsSection`.

### 3.2 Add a campaign (kit `Wizard`, title "Add a campaign")

Steps, one question each, answered by choosing where the step is one choice:
1. **Organization** (only when the person can choose).
2. **Audience**: the organization's audiences with subscriber counts. None: the step says so and the floor's act is "Add an audience".
3. **Template**: published templates first, drafts marked "Draft". **Create new template** (text act on the floor) opens VS Code "New email" for this organization; the step then waits (tone busy, "Waiting for the new template") until a template appears in the live feed and is published, and selects it.
4. **Sender**: the organization's active senders. None: "Set up sending" when this person may, otherwise who can.
5. **When**: Send now, Schedule (date and time, the kit's date-time field), or Repeat every N weeks from a date and time.
6. **Review**: the campaign in one sentence and its facts. Floor acts: **Send a test** (text; to the testing audience, or to the person's own address when none is set), **Save as draft** (text), and the primary **Send now** / **Schedule** / **Start repeating**, which confirms in the bar naming the audience size.

### 3.3 Campaign page

Head (name, back to Campaigns), Facts (audience and template as links, sender, organization, schedule or repeat), results (delivery bar, opens and clicks, breakdown, who got it with Refine), latest test. One `ActionBar`:

| State | Acts (primary last) |
|---|---|
| draft | Edit (text), Send a test (text), Send now |
| scheduled | Cancel (text, confirms), Send now |
| sending | Pause |
| paused | Cancel (text, confirms), Resume |
| sent / cancelled / failed | Duplicate (text) |

### 3.4 Automations

- **Flow** (`apps/email/automations/Flow.tsx`): a vertical spine of cards, trigger first. Card kinds: Trigger ("When someone joins Newsletter"), Send ("Send Welcome 2"), Confirm ("Send Welcome 1 and wait for confirmation, up to 7 days"), Wait ("Wait 3 days"), and End. In a draft or paused automation, a quiet "+" between cards inserts a step (Send, Confirm, Wait) and selecting a card opens its fields beside the flow (template, sender, wait length); each card can move up or down or be removed. Keyboard reaches every card and insertion point. Live counts per card (enrolled, waiting, done) come from enrollments.
- **Add an automation** (Wizard): **Trigger** (Someone joins an audience / Someone confirms their subscription / Something happens in MemQL), **Audience** (for the first two), **Steps** (the Flow, starting with one Send), **Review** (validation: templates published, confirm templates carry `{{confirmUrl}}`, senders active). Acts: Save as draft (text), **Turn on** (primary). "Something happens in MemQL" continues as the existing rule builder's questions (concept, created/changes, condition, recipients, template, sender), one per step.
- **Describe it instead** (link on the Trigger step, as Fleet's Rules): a text box, then **Draft it** -> the proposal drawn as the same Flow (marked as a proposal) -> **Check it** (server validation) -> **Turn on**, or **Edit as steps** to continue in the wizard. A refusal is shown verbatim and the words are kept.
- Rules created before this change appear in the list and their page draws the same Flow (trigger and one send).

### 3.5 Corrections to the existing screens

Every audit item in `clients/os/src` is fixed: the custom `JourneyTrail` stepper and the Journey are deleted; every Add runs `Wizard`; every lifecycle surface has one `ActionBar` with absent (never disabled) acts; loading is skeletons, never warn notices; the Email preview keeps the template's styles in a sandboxed frame (LocalTabs Formatted / Plain text); "Updates automatically" captions, developer-voice sentences and internal facts (Bundle, Runs as) are removed; vocabulary is one word per thing (organization, audience, template, sender, archived); Audience and Template facts link to their pages; panel actions sit on the right edge; the dead merge-tag helpers and orphan CSS are removed; Email gets its own icon (Mail) now that the app is one.

## 4. Automations: engine

### 4.1 Concepts (`dsl/campaigns`)

- `journey` (composite tier): `ownerUserId!, accountId!, name!, description, trigger object! ({kind: "joined"|"confirmed", audienceId}), steps []object! ([{id, kind: "send"|"confirm"|"wait", templateId?, senderIdentityId?, days?, hours?, timeoutDays?}]), status (draft|active|paused|archived)!, activatedAt, lastError`.
- `journeyEnrollment` (engine tier, server-written): `journeyId!, accountId, ownerUserId!, recipientId!, emailDigest!, stepId, stepIndex int!, nextAt, status (active|waiting|completed|exited|failed)!, exitReason, enrolledAt!, completedAt, lastError, attempts`. Id: `sha256(journeyId, recipientId)` (one enrollment per person per automation).
- Builtins: `campaignSaveJourney` (create/update with exact revision; editable in draft and paused), `campaignSetJourneyState` (turn on, pause, archive; turning on validates), `campaignDescribeJourney` (section 4.5), queries `journeys`, `journeyById`, `journeyEnrollmentCounts`.

### 4.2 The drain (`component/campaigns/journey.go`)

Every 15 seconds, the pattern `drainNewsletterWelcomes` uses (advisory-lock gates, receipts, cross-replica safe by construction):
1. **Enroll**: for each active journey, recipients of its audience whose `createdAt` (joined) or `confirmedAt` (confirmed) is after `activatedAt` and who have no enrollment.
2. **Advance** due enrollments (`status active, nextAt <= now`): Send -> `sendToRecipientOnce` with request id `journey:<enrollment>:<step>`; Confirm -> send the confirmation and set `status waiting` until the recipient confirms (then advance) or `timeoutDays` pass (exit "not confirmed"); Wait -> `nextAt = now + duration`. Past the last step -> completed. Unsubscribed, suppressed or bounced -> exited with the reason.
3. Pause stops advancing; archive exits every open enrollment ("automation retired"). A step removed while paused is skipped by id.

### 4.3 Double opt-in

- `audience.confirmation` (`none`|`required`, default none). `recipient.subscriptionStatus` gains `pending`; `recipient.confirmedAt`.
- A signup (storefront form, or Add one address with "ask them to confirm") into a confirmation-required audience creates a `pending` recipient. Pending recipients receive only a Confirm step's email; every other send skips them. The consent `grant` (source `double_opt_in`) is recorded at confirmation.
- `newsletterBinding.templateId` becomes optional; with none, the binding sends no welcome of its own.

### 4.4 Email-link pages (approved 2026-10-11)

Declared in `component/server` beside `UnsubscribePaths` (exact paths, token in the query, HMAC tokens under their own context labels, minted from the unsubscribe key ring):

| Path | Merge tag | GET | POST |
|---|---|---|---|
| `/email/confirm` | `{{confirmUrl}}` | "Confirm your subscription to <organization>?" with a Confirm button | pending -> subscribed, `confirmedAt`, consent grant; "You're subscribed." |
| `/email/preferences` | `{{preferencesUrl}}` | the address's state for that organization with Unsubscribe or Subscribe again | changes the state with a consent event |
| `/email/view` | `{{viewUrl}}` | the message as sent to that person, from the frozen template revision | (none) |

Pages are minimal, branded with the organization name only, no scripts.

### 4.5 Describe an automation

`campaignDescribeJourney({accountId, sentence})` follows `component/memql/policy_assist.go`: a `@template` workflow reads a catalog (the organization's audiences, templates with state and whether they carry `{{confirmUrl}}`, senders), calls the prompt `composeEmailJourney` with a strict schema `{name, trigger, steps[], explanation, action: "draft"|"unsupported"}`, validates the result with the same validator `campaignSaveJourney` uses (without writing), and returns the proposal. The client draws it as a proposal; saving goes through `campaignSaveJourney`. A sentence naming a template that does not exist yields a proposal whose Send card says which template to create.

## 5. Corrections to the shipped backend

| # | Defect | Fix |
|---|---|---|
| F1 | Rule activation arms the automation only on the activating node | Broadcast promote/demote as the durable path does; cluster test proves another replica fires |
| F3, F10 | Operational lane sends raw subject and text, ignores HTML, merge tags and template state | Render through the campaign renderer; require `ready` |
| F4 | Audience-mode rules read the first 100 recipients and drop rate-limited sends | Page through the cursor; rate-limited sends are retried by the drain |
| F5 | `row_address` enrollment omits `accountId` | Pass the rule's organization |
| F6 | Footer says "you subscribed to <subject>" for single sends | Sender identity's display name, then the organization name |
| F7 | Non-campaign deliveries collapse into one ledger row per person | Ledger key includes the source (campaign, rule, journey step, welcome) |
| F8 | A failed send job leaves the campaign "sending" | Stamp the campaign failed with the reason |
| F9 | Each signup scans the whole roster | Look up by address digest |
| F11 | Footer and pixel are appended after `</html>` | Insert before `</body>`; a template that carries `{{unsubscribeUrl}}` gets no second footer |
| E4 | `MEMQL_EMAIL_APP_ENABLED` hides the whole app | Removed from engine, OS and the instance overlay |

## 6. Materializer app and Files

- **Starting setup** (sub-project 1) is the contract. The Compose section becomes the workbench: **Ingredients** (files from the Library with a search picker, records, queries; each a card that can be removed, each marked "read as content" or "use as reference"), **Describe** (the statement), **Make** (format chips; Email shows the layout cards; name; folder). The ActionBar carries the state and **Materialize**. The result appears as a card with **Open** (VS Code) and, when the setup carried `returnTo`, **Back to <app>**.
- **Files**: the Add menu gains **New Markdown file**, opening the Materializer with `{format: "markdown", folderId, returnTo: files/browse}`.
- Audit fixes in the Materializer: detail through a peer tab with no Back, the inline "Bind a file" form, raw `<select>` in Refine, `Check` for preferences, standing explanatory copy, empty copy while loading.

## 7. Testing and acceptance

- **Go**: `emaildoc` golden renders for every block and layout; parse refusals; `emailRender`; the composer merge (brand untouched, unknown ids refused); journey drain with the CI Postgres image (enroll, advance, confirm, timeout, pause, archive, unsubscribe exit, exactly-once across two drains); the three link pages (token forgery, GET does not change state, POST does); every F-fix with a test that fails before it; F1 in `test/clustere2e`.
- **OS** (vitest): wizard flows, ActionBar acts per state (absent, never disabled), Flow editing by keyboard, Describe-it-instead states, intent round trip, the guard tests from sub-project 1, navigation contract regenerated.
- **VS Code**: New email page states, editor selection and edits, publish, save as layout, jsdom tests for the new page scripts.
- **Fidelity harness** (`scripts/email/fidelity.sh`, local only): renders documents describing the owner's eight examples (kept outside this repository, path from `MEMQL_EMAIL_FIDELITY_DIR`), screenshots both with headless Chrome at 600px and reports a per-email pixel difference.
- **Live**: on the owner's local cluster (only after the owner says go): create the client's layouts from the examples' resources, write a new Stage email by description, publish, build the Welcome-and-Stage automation by hand and by description, sign up through a storefront form, confirm, and read the mail in the capture inbox.

## Out of scope

- Postfix transport, Event Grid delivery feedback (#6037) and installer email authorization (#6036) stay in their issues.
- Branching automations (if/else) and event-triggered multi-step journeys.
- A/B tests and per-recipient time zones.
