# VS Code email editor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Write an email in the MemQL productivity tool. A **New email** page (organization, layout, name, describe) creates the `*.email.json` through the Materializer, and the redesigned email editor shows the email as it is sent: click a block in the live preview to edit it, with Source, Publish and Save as layout.

**Architecture:** Two webview pages, each a pure page builder (`newEmailPage.ts`, `emailPage.ts`) plus a bundled view script, styled only with `--vscode-*` variables in the Markdown editor's idiom. A `new-email` resource kind (`memql-file://<domain>/new-email/<bareAccountId>/New%20email`) opens the New email page as a read-only custom editor, so the same link works from the palette, the desktop URI handler and the browser editor. The `*.email.json` custom text editor keeps one buffer for Design and Source: the inspector sends semantic edits that the host applies as one minimal text edit, and the preview is the engine's `emailRender` output in a sandboxed, double-buffered frame. Creation goes only through the SDK `startMaterialize` / `followComposition` helper; publishing uses `campaignSaveTemplate` keyed by the file (`sourceArtifactId`), and layouts use `campaignSaveLayout`.

**Tech Stack:** TypeScript (`editors/productivity`, node:test, jsdom 26, esbuild), VS Code API 1.91+, the browser editor handoff (`editors/browser`), the core extension's gallery harness (`editors/vscode/gallery`), one env-gated Go test in `component/compose/emaildoc` for gallery fixtures, headless Chrome for gallery shots.

**Spec:** `docs/superpowers/specs/2026-10-11-email-layouts-automations-design.md` section 2 (binding), 1.1 to 1.6 and the VS Code bullets of section 7. Also read `docs/superpowers/plans/2026-10-11-email-layouts-engine.md` (Task 1 and Rulings for the document format; Tasks 7, 8, 9, 11, 12 for the builtins this plan consumes), `docs/superpowers/specs/2026-10-11-materializer-front-door-design.md` (amendments 1 to 5) with `docs/superpowers/plans/2026-10-11-materializer-front-door.md` Tasks 4, 5 and 7, and `docs/internal/design/2026-09-28-vscode-extension-ux.md`.

**Tracking:** sub-project #6032 (program #6022). Every commit message references #6032.

## Sequencing

- **Starts after** the layout engine plan's Tasks 7, 8, 9 and 12 have landed on `epic/email-rework`. They regenerate the SDK builders this plan calls: `buildEmailLayouts`, `buildEmailRender`, `buildCampaignSaveLayout`, `buildTemplateForSource`, and `ComposeMaterializeArgs.layoutId`.
- **The front door comes first.** Engine Task 12 merges `epic/materializer-front-door` into this branch; Task 1 here re-checks it and merges again if that branch moved. This plan needs its Task 4 (`sdk/ts/src/client/materialize.ts`: `startMaterialize`, `followComposition`, `materialize`, `MaterializeCancelled`, `MaterializeFailed`), Task 5 (`editors/productivity/src/newMarkdown.ts`; `templateExamples.ts` on the helper) and Task 7 (`editors/productivity/test/onePathCreate.test.ts`).
- **Engine Task 11 is superseded.** It only taught the old preview to read layout documents. If it has not run, skip it (Task 1 records that in its commit body); if it has, Tasks 2 and 7 here replace its code.
- **The OS plan** adds `editorNewEmailURL` in `clients/os/src/items/editorPreference.ts` (the third validator, with `clients/os/test/files/editor-handoff.test.ts`) and the campaign wizard's "Create new template" act. The link contract is defined here (Task 3, Rulings 1).
- Run the tasks in order: each builds on the ones before it. The old email files stay compiling until the task that deletes them (Task 7 for the editor and publisher, Task 9 for the examples flow).

## Global Constraints

- Pre-release: no backwards-compat shims, no fallback code paths. Delete replaced code in the task that replaces it and fix every caller in the same commit.
- Stage files by explicit path (`git add <file>`); never `git add -A` / `git add .`. Write each commit message to a file and run `git commit -F <file>`. Another writer may commit in this worktree: if `git commit` reports `index.lock`, wait a few seconds and retry.
- Clients use bare ids only. Layout ids (`builtin:<name>`, `org:<id>`) are opaque: pass an `emailLayouts` row's `id` to `composeMaterialize.layoutId` and its `layoutId` to `campaignSaveLayout`; never build or split them.
- **One creation path.** A new file is created only through the SDK helper. Uploads of bytes the person already has (references, a picture, a local email saved to MemQL) go through `api.uploadFile` or `Documents.createFile`, and only from modules on the productivity guard's allowlist (Task 9 updates it).
- **Copy** follows the UX record's voice: sentence case, no emojis, a status line at most six words, one sentence plus the fix for errors, typographic apostrophes (’) as in the rest of Productivity Tools, no "receipt", "Materializer", "Nexus" or "Campaigns" in user strings, no `--` as punctuation. Every page string lives in `src/emailCopy.ts`.
- **Styling** uses only `--vscode-*` variables, no `--memql-*` tokens and no kit, the CSP pattern of `markdownPage.ts` (nonce scripts, `style-src 'unsafe-inline'`) plus `img-src data:` and `frame-src 'self'`. Nothing loads from the network. Reduced motion is respected, every control is reachable by keyboard, targets are at least 34 px, and focus is visible.
- **Pages never repaint on updates.** Host messages patch regions; a field with focus is never overwritten; drafts and page state persist (webview restore plus `workspaceState`).
- **An act that is not legal is absent, never disabled.** State lives once, in the action bar.
- **Commands** carry the category `MemQL`, titles without a `Word: ` prefix, and editor-title entries carry codicons.
- **Commands to run.** Once per worktree (disk is tight: one `npm ci` per worktree at a time): `bash scripts/vscode/deps.sh && npm ci --no-audit --no-fund --prefix editors/vscode && npm --prefix editors/vscode run compile && npm ci --no-audit --no-fund --prefix editors/productivity`. Unit tests `cd editors/productivity && npm test`. Typecheck and bundle `cd editors/productivity && npm run compile`. Host suite `node editors/productivity/test/runner.cjs` (web) and `node editors/productivity/test/runner.cjs --desktop` (add `MEMQL_VSCODE_VERSION=1.91.0` for the minimum engine). Browser editor `npm ci --no-audit --no-fund --prefix editors/browser && npm --prefix editors/browser test`. Core extension `cd editors/vscode && npm test` when its test helpers change. SDK `cd sdk/ts && npm test && npm run build`.
- **This repository is public.** Fixtures, gallery pages and tests use the built-in Acme layouts only; no client copy, pictures, colours or fonts.
- No wire changes are made here. Commit bodies name the builtins a task starts consuming.

## Rulings

Decisions on points the spec leaves open or gets wrong. Every task implicitly includes these.

1. **The New email link** is `memql-file://<domain>/new-email/<bareAccountId>/New%20email`: three path segments, no query. The account segment uses the id grammar `^[\w-]{1,160}$`; the name segment is the tab title. Spec 2.1's `memql-file://<domain>/new/email?account=<id>&returnTo=email` is not used: all three validators refuse a query, and `returnTo` means nothing to VS Code (the OS wizard waits on its own live feed). Spec 2.1 needs that sentence amended.
2. **New email is a read-only custom editor** (`memql.productivity.newEmail`, selector `memql-file:/new-email/*/*`). VS Code matches a selector containing `/` against `<scheme>:<path>`, case-insensitively (read in VS Code 1.108's workbench, `globMatchesResource`; the 1.91.0 host run in Task 9 proves the minimum engine), so the pattern matches only these links, never a local folder that happens to be called `new-email`. `vscode.open` of the link then works on every host, and VS Code restores the tab after a reload. `MemQLFiles` answers `stat` and `readFile` for these links without a network call and refuses writes.
3. **Organization on New email.** The Organization select appears only when the person has more than one active organization. It preselects the link's organization when it is one of theirs. The palette command opens the link for the organization last used on that cluster (`globalState` key `memql.newEmail.organization:<domain>`), else the first by name.
4. **Act styles are fixed per act.** Publish, Write it, Connect, Open Source, Save to MemQL and Retry are buttons; Save as layout and Cancel are text acts. "Start from the layout" is a text act beside Write it and the button when it is the only act (the UX record: one button, primary last).
5. **The email's organization** is needed before any publish, because `emailRender` requires `accountId`. It is resolved, first answer wins: a template resource's organization; the composition that wrote the file (`compositionForOutputFile`; an email composition carries exactly one organization); the Library entry's organization label when it has exactly one (`accountIds` on the artifact); the person's only active organization. Otherwise the inspector shows an Organization select. A choice for a Library file with no labels is written as its label (`setArtifactAccounts`), so the file knows it from then on; any other choice is kept with the page state. **Publish never prompts.** (Spec 2.2 says Publish asks on first publish; with a server-rendered preview the question has to come earlier.)
6. **One renderer.** The Design preview is `emailRender` output (preview mode, `data-block` marks), cleaned by `previewHTML` and shown in `sandbox="allow-same-origin"` `srcdoc` frames: scripts cannot run, and the page can read clicks. Two frames swap on load so an update never flashes or loses scroll. Without a connection, Design shows the document's skeleton (layout mode) or its plain text (custom mode) and the state "Not connected"; there is no client-side rendering path, so the old offline preview of local HTML templates is gone.
7. **Edits.** The view sends semantic operations (`EmailEdit`); the host applies `editEmail` to the current text and writes one `minimalEdit` span, so undo works and a Source editor beside it keeps its cursor. Text fields send after 250 ms idle or on blur, so a burst of typing is one undo step. Edits are refused while the text is not valid JSON.
8. **What the inspector edits:** subject and preheader; per block the words, button label, links, picture (choose from Files, upload, remove) and its description, colours (theme tokens only), alignment, spacer height, social and footer links, footer lines, an `html` block's markup, and the role switch "Same in every email". Sizes, spacing and padding stay in Source because they come from the layout. Add after offers the leaf types Heading, Text, Highlight, Button, Picture, Divider and Space; moves stay within a container.
9. **Action bar of the editor** (first match wins): busy (Publishing / Saving the layout / Saving to MemQL, no acts); not connected (Connect); unreadable (Open Source); organization unknown (no acts; the inspector asks); local file (Save to MemQL); issues (Fix N issues to publish: Save as layout); unsaved (Save as layout, Publish); published and unchanged (Published to <organization>: Save as layout); published and changed (Changes since publishing: Save as layout, Publish); otherwise Draft (Save as layout, Publish). Save as layout is absent for custom HTML. Full table in Task 6.
10. **Publish** saves a dirty file first, then calls `campaignSaveTemplate` with `action: "save"` (no `templateId`, `sourceArtifactId` = the artifact id, `name` = the file name without `.email.json`, `expectedRevision` from `templateForSource` or empty) and then `action: "publish"` with the receipt's `templateId` and `revision`. It stops when the save reports issues, never opens a tab, and keeps no client-side mapping: the server finds the template by its source, which also recovers a lost response. A template resource publishes with its opened revision.
11. **Save as layout** updates the organization layout saved from this file (an `emailLayouts` row whose `sourceArtifactId` matches) and otherwise creates one under a `layoutId` persisted before the request. The name is asked inline in the action bar, prefilled with that layout's name or the file name. Custom-mode emails cannot be layouts.
12. **Local `*.email.json` files** can be designed but publish only from MemQL: their act is **Save to MemQL**, which saves and uploads the file (bytes the person has), labels it with the chosen organization and reopens it in place. This keeps the old "Use in Campaigns" capability for local files without a local mapping.
13. **Custom-mode documents** show the rendered HTML, the Email group (subject, preheader) and Preview with; no outline and no Theme. The HTML is edited in Source; the inspector's "Edit HTML" opens Split. (Spec 2.2's "open with the rendered HTML and the Source editor" is read this way; no editor opens itself.)
14. **Resume is part of New email.** The page stores the full request (and the composition id once known) in `workspaceState` before starting. A restored or reopened page for the same link resumes following and opens the result once. Closing the tab stops following without cancelling. This replaces "Open Last Email Composition".
15. **Recipes are not offered in VS Code.** Recipes do not carry a layout, so a recipe saved here would re-run with the wrong layout. The Materializer app keeps creating and running recipes.
16. **Copy lint.** All page copy lives in `emailCopy.ts`; a productivity twin of the core copy lint scans it, the productivity manifest and the productivity sources.
17. **Gallery fixtures come from the Go renderer** through one env-gated test (`TestWriteEditorGalleryFixtures`), never committed, so gallery pages show real renders.

## Design direction (frontend-design: plan, review, build, critique)

**Subject and job.** A moderately technical MemQL user writes a client organization's campaign email. New email's job: decide how the email starts (which organization, which layout, from scratch or described). The editor's job: see the email exactly as sent and change it by pointing at it. The owner's brief: minimal and clean, modelled on the Markdown editor, no capability lost, every element justified.

**Tokens** (all from the editor theme; nothing new is introduced):

| Role | Source |
|---|---|
| Ink, paper | `--vscode-editor-foreground`, `--vscode-editor-background` |
| Quiet text (meta, labels) | `--vscode-descriptionForeground` |
| Hairlines | `--vscode-panel-border` |
| Raised surfaces (bottom sheet, menus) | `--vscode-editorWidget-background`, border `--vscode-widget-border`, shadow `--vscode-widget-shadow` |
| Selection | `--vscode-focusBorder` (2 px outline), rows `--vscode-list-inactiveSelectionBackground` |
| Acts | `--vscode-button-*` for the one button, `--vscode-textLink-foreground` for text acts, `--vscode-toolbar-hoverBackground` for hover |
| Problems | `--vscode-errorForeground` |

**Type.** The editor's UI font (`--vscode-font-family`) only: 13 px/1.6 for body and fields, 12 px for meta and field labels, 14 px weight 600 for inspector subheads, 24 px/1.3 weight 600 with -0.025em tracking for the New email title (the Markdown empty page's heading). Monospace (`--vscode-editor-font-family`) only in the HTML block field and the Plain text view.

**Shape and motion.** 6 px radius on controls (the Markdown editor's), 8 px on layout thumbnails; no shadows except VS Code's widget shadow on the sheet and menus. The only animation is the skeleton sweep (none under reduced motion); preview swaps are instant.

**The one bold element: the email itself.** In Design the preview fills its column with the email's own page colour at real size; hovering outlines a block, clicking selects it with a 2 px focus-colour outline that the inspector follows. On New email the layout cards are scaled renders of real emails. Everything around them is the Markdown editor's quiet chrome.

**Review against the brief** (defaults rejected): numbered steps on New email (the page is one form read top to bottom; numbers add nothing); a picker chain for organization and name (in-page fields instead); boxes around inspector groups (subheads are the containers, as the UX record says); a device frame around the mobile preview (a 375 px column with a hairline says it); an accent colour for selection beyond the theme's focus colour; status sentences in the body (state lives once, in the action bar).

**Critique** happens in Task 11 against the rendered gallery (checklist there), before the owner's hand test (Task 13).

## Review Focus

1. **Clicking inside the preview never navigates or runs anything**: links are stripped, the frame has no `allow-scripts`, and the click handler prevents default. Tests in Tasks 4 and 5.
2. **Typing in a field while a document update arrives** keeps the caret and the typed text. Test in Task 5.
3. **Double submit**: two clicks on Write it make one composition; two clicks on Publish make one publication. Tests in Tasks 6 and 9.
4. **Republishing after a lost response** updates the same template with no client-side mapping. Test in Task 6.
5. **A New email link with a query, a canonical id or another cluster's domain** is refused by both validators. Tests in Task 3.
6. **Closing New email mid-write** does not cancel; reopening resumes and opens the result exactly once. Test in Task 9.

## Inventory (every capability before, and where it is after)

| Before | After |
|---|---|
| New Email Template (an untitled JSON file with a sample) | New Email: Start from the layout |
| Create Email from Examples (organization, references, pictures to include, name, brief) | New Email: Describe it, with references and "Use in the email" pictures, then Write it |
| Save as recipe and create draft | Not in VS Code (Rulings 15); recipes stay in the Materializer app |
| Open Last Email Composition | Reopening New Email for the organization resumes it; the page restores after a reload (Rulings 14) |
| Preview Email Template | Email Split View |
| Email Template Source / Preview / Split View | Email Source / Email Design / Email Split View (with codicons) |
| Preview: Sample values | Design: Preview with (persisted per email) |
| Preview: Plain-text version | Design: the Plain text width option |
| Use in Campaigns: Save draft in Campaigns | The file is the draft (Save); a template resource still saves as a draft as before |
| Use in Campaigns: Publish template for campaigns | Publish (action bar), in place |
| Publishing a local email file | Save to MemQL, then Publish (Rulings 12) |
| Offline preview of local HTML templates | Not kept (Rulings 6); Source still works offline |
| (new) | Layout cards, block outline (select, move, add, remove), block fields, pictures from Files or upload, Theme, Mobile width, Save as layout, issues listed by block |

---

### Task 1: Bring in the front door and confirm the engine surface

Read first: the Sequencing section above; `sdk/ts/src/client/materialize.ts` and `sdk/ts/test/materialize.test.ts` as merged.

**Files:**
- Merge: `epic/materializer-front-door` (only when it is not already an ancestor of `HEAD`)
- Modify (only when missing): `sdk/ts/src/client/materialize.ts` (`MaterializeRequest.layoutId?: string`)
- Test (only when the field was missing): `sdk/ts/test/materialize.test.ts`

**Interfaces:**
- Consumes: front-door Task 4 (`startMaterialize`, `followComposition`, `materialize`, `MaterializeCancelled`, `MaterializeFailed`, `MaterializeRequest`, `MaterializeSource`); engine Tasks 7 to 9 and 12 (generated builders).
- Produces: `MaterializeRequest.layoutId?: string`, passed through to `composeMaterialize` by `startMaterialize` (it already forwards every non-empty field).

- [ ] **Step 1: Check the preconditions**

```bash
git merge-base --is-ancestor epic/materializer-front-door HEAD && echo "front door merged" || git merge --no-ff epic/materializer-front-door
grep -nE "export (async )?function (startMaterialize|followComposition|materialize)\b|export class Materialize(Cancelled|Failed)" sdk/ts/src/client/materialize.ts
grep -nE "export function build(EmailRender|EmailLayouts|CampaignSaveLayout|TemplateForSource)\(" sdk/ts/src/client/generated_builtins.ts sdk/ts/src/client/generated_queries.ts
grep -n "layoutId" sdk/ts/src/client/generated_builtins.ts | head -3
test -f editors/productivity/src/newMarkdown.ts && test -f editors/productivity/test/onePathCreate.test.ts && echo "front door editor pieces present"
grep -n "layoutId" sdk/ts/src/client/materialize.ts || echo "MaterializeRequest needs layoutId"
```

Expected: every grep prints matches. If a builder or the front-door editor pieces are missing, stop: the plan this one depends on is not finished. A merge conflict is resolved by keeping both sides.

- [ ] **Step 2 (only when `layoutId` is missing): failing test** in `sdk/ts/test/materialize.test.ts`, reusing that file's `fake` helper:

```ts
test("an email layout passes through to composeMaterialize", async () => {
  const f = fake({
    composeMaterialize: [[{ compositionId: "c1" }]],
    compositionById: [[{ status: "ready", outputFileId: "f1" }]],
    libraryArtifactForFile: [[{ id: "a1", title: "Welcome.email.json" }]],
  });
  await materialize({ execute: f.execute, intervalMs: 1 }, { name: "Welcome", format: "json", outputKind: "email_template",
    accountIds: ["client-org"], layoutId: "builtin:letter", requestId: "r1" });
  assert.match(f.calls[0].call, /layoutId:\s*"builtin:letter"/);
});
```

Run: `cd sdk/ts && npm test`
Expected: FAIL (`layoutId` does not exist on `MaterializeRequest`).

- [ ] **Step 3: Implement** `layoutId?: string` on `MaterializeRequest`, then `cd sdk/ts && npm test && npm run build`. Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add sdk/ts/src/client/materialize.ts sdk/ts/test/materialize.test.ts
git commit -F <message file>   # "feat(sdk): materialize passes an email layout through (#6032)"; body: whether engine Task 11 ran or is skipped as superseded by this plan
```

If nothing changed (the merge was already in and `layoutId` existed), there is no commit; note "engine Task 11 superseded" in Task 2's commit body instead.

---

### Task 2: The email document model

Read first: engine plan Task 1 (format, the per-type key table, value rules) and Rulings 2, 7, 13; `editors/productivity/src/templates.ts` (being replaced in Task 7); `editors/productivity/src/documents.ts` `read` (template branch).

**Files:**
- Create: `editors/productivity/src/userError.ts` (`UserInputError` moves here so view bundles stay free of the SDK)
- Modify: `editors/productivity/src/problems.ts` (re-export `UserInputError` from `./userError.js`; nothing else changes)
- Create: `editors/productivity/src/emailDocument.ts`
- Modify: `editors/productivity/media/email.schema.json` (generated by the test below)
- Modify: `editors/productivity/src/documents.ts` (template rows that carry `document`; `OpenDocument.accountIds`)
- Test: `editors/productivity/test/email-document.test.ts` (create), `editors/productivity/test/documents.test.ts`

**Interfaces:**
- Produces (exact):

```ts
// emailDocument.ts — pure, no vscode, no SDK.
export const THEME_TOKENS = ["page", "canvas", "primary", "secondary", "accent", "tint", "text", "muted", "white"] as const;
export type ThemeToken = typeof THEME_TOKENS[number];
export const LINK_TAGS = ["{{unsubscribeUrl}}", "{{preferencesUrl}}", "{{viewUrl}}", "{{confirmUrl}}"] as const;
export const BLOCK_TYPES = ["image", "heading", "text", "highlight", "button", "divider", "spacer", "section", "columns", "social", "footer", "html"] as const;
export type BlockType = typeof BLOCK_TYPES[number];
export interface EmailTheme { page: string; canvas: string; primary: string; secondary: string; accent: string; tint: string; text: string; muted: string; headingFont: string; bodyFont: string; webFonts?: boolean }
export interface EmailColumn { width: number; cellPadding?: number; blocks: EmailBlock[] }
export interface EmailBlock { id: string; type: BlockType; role?: "brand" | "content"; blocks?: EmailBlock[]; columns?: EmailColumn[]; [field: string]: unknown }
export interface LayoutEmail { subject: string; preheader?: string; layout?: string; theme: EmailTheme; blocks: EmailBlock[] }
export interface CustomEmail { subject: string; preheader?: string; textBody: string; htmlBody: string }
export type EmailDocument = LayoutEmail | CustomEmail;
export type EditableField = "text" | "label" | "href" | "alt" | "src" | "color" | "background" | "align" | "height" | "title" | "links" | "lines" | "html";
export type AddableType = "heading" | "text" | "highlight" | "button" | "image" | "divider" | "spacer";
export type EmailEdit =
  | { kind: "subject"; value: string }
  | { kind: "preheader"; value: string }
  | { kind: "field"; block: string; field: EditableField; value: string | number | string[] | Array<Record<string, string>> }
  | { kind: "move"; block: string; by: -1 | 1 }
  | { kind: "add"; after: string; type: AddableType }
  | { kind: "remove"; block: string }
  | { kind: "role"; block: string; brand: boolean }
  | { kind: "theme"; key: Exclude<keyof EmailTheme, "webFonts">; value: string };

/** Every key a block may carry, by type: engine plan Task 1's table, plus id/type/role, plus padding except on spacer. */
export const BLOCK_FIELDS: Readonly<Record<BlockType, readonly string[]>>;
/** The fields the inspector edits, by type, in display order (Rulings 8). */
export const EDITABLE_FIELDS: Readonly<Record<BlockType, readonly EditableField[]>>;
export function readEmail(text: string): EmailDocument;
export function isLayoutEmail(doc: EmailDocument): doc is LayoutEmail;
export function writeEmail(doc: EmailDocument): string;                       // JSON.stringify(doc, null, 2) + "\n"
export function walkBlocks(blocks: readonly EmailBlock[], visit: (block: EmailBlock, depth: number, siblings: EmailBlock[], index: number, brand: boolean) => void): void;
export function findBlock(doc: LayoutEmail, id: string): { block: EmailBlock; siblings: EmailBlock[]; index: number; depth: number; brand: boolean } | undefined;
export function canMove(doc: LayoutEmail, id: string, by: -1 | 1): boolean;
export function blockLabel(block: EmailBlock): string;                         // "Heading · Thanks for joining us"
export function editEmail(doc: EmailDocument, edit: EmailEdit): EmailDocument;  // a new document; UserInputError on refusal
export function addedBlockId(doc: LayoutEmail, edit: Extract<EmailEdit, { kind: "add" }>): string;  // the id editEmail will give it
export function validLink(href: string): boolean;                             // engine Rulings 7
export function sampleKeys(doc: EmailDocument): string[];                     // supported merge tags in use, sorted, at most 64
export function sameEmail(a: EmailDocument, b: unknown): boolean;
export function unknownFields(doc: LayoutEmail): string[];                    // `block "<id>": <key>`
export function emailSchema(): Record<string, unknown>;
export function minimalEdit(before: string, after: string): { start: number; end: number; text: string };
```

- `documents.ts`: `OpenDocument` gains `accountIds?: string[]` (artifacts only, from `libraryArtifactById`'s `accountIds`); a template row whose `document` is an object opens as `JSON.stringify(document, null, 2) + "\n"`; a row without one opens as custom mode `{subject, preheader?, textBody, htmlBody}` (`preheader` only when non-empty).

Exact sentences (`UserInputError`, rendered verbatim):

| Case | Message |
|---|---|
| over 2 MiB | `Emails must be 2 MiB or smaller.` |
| not JSON | `This email has invalid JSON. Open Source and correct its formatting.` |
| not an object | `An email must be a JSON object.` |
| both modes | `An email is either a layout (blocks) or custom HTML (textBody and htmlBody), not both.` |
| neither mode | `An email needs blocks (a layout) or textBody and htmlBody (custom HTML).` |
| unknown top-level key | `Remove "<key>" in Source. Emails don’t have that field.` |
| subject | `Enter a subject on one line.` / `The subject must be under 999 bytes.` |
| custom bodies | `The email needs textBody and htmlBody strings.` / `Include a plain-text version.` |
| layout shape | `A layout email needs a theme and a list of blocks.` |
| block shape | `Every block needs an id and a type.` / `Block "<id>" has a type MemQL doesn’t know: "<type>". Fix it in Source.` |
| edit: block gone | `That block is no longer in the email.` |
| edit: colour | `Choose one of the theme colours.` |
| edit: link | `Enter a web address, an email address (mailto:), a phone number (tel:) or a link tag such as {{unsubscribeUrl}}.` |
| edit: theme colour | `Enter a colour as #rrggbb.` |
| edit: font | `Enter a font name using letters, digits, spaces or hyphens.` |
| edit: move | `This block can’t move further.` |

Block keys are not validated beyond `id`/`type`: unknown keys are kept untouched (the server's `Parse` and the Source schema are the validators). `add` defaults (exact): heading `{id, type, text: ""}`, text `{id, type, text: ""}`, highlight `{id, type, text: ""}`, button `{id, type, label: "Button", href: ""}`, image `{id, type, src: "", alt: "", width: 552}`, divider `{id, type}`, spacer `{id, type, height: 24}`; ids are `<type>-<n>` with the smallest unused `n` from 1. An empty value for an optional field (`href`, `alt`, `title`) deletes its key; `role` brand sets `"brand"`, off sets `"content"` when the effective role would otherwise be brand (inside a brand container) and deletes the key otherwise. `sameEmail` compares after normalizing both sides: keys sorted; keys whose value is `""`, `false`, `null` or an empty array dropped; numeric `0` dropped for the Go non-pointer `omitempty` keys `size lineHeight listLineHeight weight width height paddingY maxWidth stackBelow after`. `emailSchema()` is draft-07: `oneOf` layout and custom; a block definition whose `allOf` holds one `if {type: const} then {properties, additionalProperties: false}` per type, with colour keys as the `THEME_TOKENS` enum, `align` enum, `font` enum, `weight` enum `0|400|700`, `id` and `src` patterns from engine Task 1, `href` as the `validLink` pattern, and `$ref` for nested `blocks` and `columns`.

- [ ] **Step 1: Failing tests** (`test/email-document.test.ts`):

```ts
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, readdirSync, writeFileSync } from "node:fs";
import { readEmail, writeEmail, editEmail, minimalEdit, sameEmail, sampleKeys, emailSchema, unknownFields, blockLabel, canMove, isLayoutEmail, type LayoutEmail } from "../src/emailDocument.js";

const theme = { page: "#eef0f3", canvas: "#ffffff", primary: "#1f5f8b", secondary: "#5b8fb9", accent: "#d9534f", tint: "#f6f4ef", text: "#2b2b2b", muted: "#8a8f98", headingFont: "Georgia", bodyFont: "Arial" };
const layout = (blocks: unknown[]) => JSON.stringify({ subject: "Hello", preheader: "Soon", layout: "builtin:article", theme, blocks }, null, 2) + "\n";

test("an email is a layout or custom HTML, never both", () => {
  assert.ok(isLayoutEmail(readEmail(layout([{ id: "headline", type: "heading", text: "Hi" }]))));
  assert.ok(!isLayoutEmail(readEmail('{"subject":"Plain","textBody":"Hello","htmlBody":""}')));
  assert.throws(() => readEmail(`{"subject":"x","textBody":"y","htmlBody":"","blocks":[],"theme":${JSON.stringify(theme)}}`), /either a layout \(blocks\) or custom HTML/);
  assert.throws(() => readEmail('{"subject":"x"}'), /needs blocks \(a layout\) or textBody and htmlBody/);
  assert.throws(() => readEmail("{"), /invalid JSON\. Open Source/);
  assert.throws(() => readEmail('{"subject":"x","textBody":"y","htmlBody":"","accountId":"c"}'), /Remove "accountId" in Source/);
  assert.throws(() => readEmail(layout([{ id: "m", type: "marquee" }])), /doesn’t know: "marquee"/);
});

test("an inspector edit changes one span and keeps every other key", () => {
  const before = layout([{ id: "headline", type: "heading", text: "Hi", size: 48, custom: "kept" }, { id: "cta", type: "button", label: "Go", href: "" }]);
  const after = writeEmail(editEmail(readEmail(before), { kind: "field", block: "headline", field: "text", value: "Welcome aboard" }));
  const span = minimalEdit(before, after);
  assert.equal(before.slice(0, span.start) + span.text + before.slice(span.end), after);
  assert.equal(span.text, "Welcome aboard");
  assert.match(after, /"custom": "kept"/);
});
```

Also, one `test(...)` each:
- `blocks move within their container and illegal moves are refused`: in `[a, section{b, c}, d]`, `canMove(doc, "b", -1)` is false, moving `c` up gives `section{c, b}`, moving `d` down throws `can’t move further`.
- `add after makes a unique id with the type's defaults and remove takes children with it`: with `text-1` present, adding text after it inserts `{id: "text-2", type: "text", text: ""}` right after; adding an image inside a section inserts `{id: "image-1", type: "image", src: "", alt: "", width: 552}` there; removing the section removes its children; `addedBlockId` returns the same ids.
- `colours must be theme colours and links must be safe`: `#ff0000` refused; `javascript:alert(1)`, `{{fields.url}}`, `https://a b` refused; `""`, `https://example.com/a?b=1`, `mailto:hello@example.com`, `tel:+15551234`, `{{confirmUrl}}` accepted; theme `primary: "red"` refused with `#rrggbb`; font `"1x"` refused.
- `the role switch sets brand, and content only where it would otherwise inherit brand`.
- `a published document compares equal despite key order and omitted empty values`: the file has `"href": ""` and `width: 300`, the stored copy has keys in another order without `href`, plus `theme.webFonts: false`: `sameEmail` is true; a changed subject is false.
- `Preview with lists the merge tags the email uses, never link tags`: subject `For {{accountName}}`, text `Hi {{displayName}} at {{fields.company}} [stop]({{unsubscribeUrl}})` gives `["accountName", "displayName", "fields.company"]`; a custom email reads subject, preheader, textBody and htmlBody.
- `blocks are named by their words`: heading `Thanks for joining us` gives `Heading · Thanks for joining us`; a text block's Markdown markers are stripped and cut at 40 characters with `…`; an image uses its alt or `Picture`; a spacer is `Space`.
- `the JSON schema in media/ is generated from the field table`:

```ts
test("the JSON schema in media/ is generated from the field table", () => {
  const generated = JSON.stringify(emailSchema(), null, 2) + "\n";
  if (process.env.MEMQL_UPDATE_SCHEMA === "1") writeFileSync("media/email.schema.json", generated);
  assert.equal(readFileSync("media/email.schema.json", "utf8"), generated, "run MEMQL_UPDATE_SCHEMA=1 npm test and review the diff");
});

test("the built-in layouts use only fields this editor knows", () => {
  const dir = "../../component/compose/emaildoc/layouts";
  const files = readdirSync(dir).filter(name => name.endsWith(".email.json"));
  assert.equal(files.length, 4);
  for (const name of files) {
    const doc = readEmail(readFileSync(`${dir}/${name}`, "utf8"));
    assert.ok(isLayoutEmail(doc), name);
    assert.deepEqual(unknownFields(doc as LayoutEmail), [], name);
  }
});
```

In `test/documents.test.ts`:
- `a template row that carries a document opens as that document` (a `templateById` row with `document: {subject, layout: "builtin:letter", theme, blocks}` opens as exactly that object);
- `a template row without a document opens as a custom email` (preheader included only when non-empty);
- `an artifact's organization labels travel with the opened file` (a `libraryArtifactById` row with `accountIds: ["client-org"]` gives `opened.accountIds` equal to that list).

- [ ] **Step 2: Run, verify failure**

Run: `cd editors/productivity && npm test`
Expected: FAIL (`emailDocument.js` missing; the documents cases fail).

- [ ] **Step 3: Implement** as specified. `userError.ts` exports `class UserInputError extends Error {}`; `problems.ts` does `export { UserInputError } from "./userError.js";` and imports it for its own use. Generate the schema with `MEMQL_UPDATE_SCHEMA=1 npm test` and read the diff: `*.email.json` files keep their `jsonValidation` entry in `package.json`.

- [ ] **Step 4: Run**

Run: `cd editors/productivity && npm test && npm run compile`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add editors/productivity/src/userError.ts editors/productivity/src/problems.ts editors/productivity/src/emailDocument.ts editors/productivity/media/email.schema.json editors/productivity/src/documents.ts editors/productivity/test/email-document.test.ts editors/productivity/test/documents.test.ts
git commit -F <message file>   # "feat(productivity): the email document model for layout and custom emails (#6032)"
```

---

### Task 3: The New email link

Read first: Rulings 1 and 2; `editors/productivity/src/documents.ts` (`Resource`, `resourceFrom`); `editors/productivity/src/extension.ts` (`MemQLFiles`, the URI handler, `open`); `editors/browser/src/handoff.mjs` (`requestedResource`) and `editors/browser/src/main.ts` (it calls `vscode.open` after `checkTools`, unchanged).

**Files:**
- Modify: `editors/productivity/src/documents.ts` (`Resource.kind` adds `"new-email"`; `resourceFrom` accepts it; Create `newEmailResource`; `Documents.read` and `save` refuse it)
- Modify: `editors/productivity/src/extension.ts` (`MemQLFiles.stat` returns a read-only empty file and `readFile` empty bytes for a New email link, without a network call; `writeFile` refuses)
- Modify: `editors/browser/src/handoff.mjs` (`requestedResource` accepts `new-email`)
- Test: `editors/productivity/test/documents.test.ts`, `editors/browser/test/handoff.test.mjs`

**Interfaces:**
- Produces: `Resource { domain; kind: "artifacts" | "templates" | "new-email"; id; name }`; `export function newEmailResource(domain: string, accountId: string): string` (lowercases the domain, validates the id with `^[\w-]{1,160}$`, returns `memql-file://<domain>/new-email/<id>/New%20email`, refuses with `Invalid MemQL file link.`).
- Contract for the OS plan's `editorNewEmailURL`: exactly the string `newEmailResource` builds.

- [ ] **Step 1: Failing tests**

`test/documents.test.ts`:

```ts
test("a New email link names an organization and opens no file", async () => {
  const link = "memql-file://client.example/new-email/client-org/New%20email";
  assert.deepEqual(resourceFrom(link), { domain: "client.example", kind: "new-email", id: "client-org", name: "New email" });
  assert.equal(newEmailResource("Client.Example", "client-org"), link);
  for (const bad of [
    "memql-file://client.example/new/email?account=client-org&returnTo=email",
    "memql-file://client.example/new-email/client-org",
    "memql-file://client.example/new-email/v1:accounts:account:client-org/New%20email",
    "memql-file://client.example/new-email/client-org/New%20email?returnTo=email",
    "memql-file://client.example/new-email/client-org/a%2Fb",
  ]) assert.throws(() => resourceFrom(bad), bad);
  assert.throws(() => newEmailResource("client.example", "v1:accounts:account:x"));
  const api = { connect: async () => { throw new Error("must not connect"); } } as unknown as EditorConnectionAPI;
  await assert.rejects(new Documents(api).read(link), /opens a page, not a file/);
});
```

`editors/browser/test/handoff.test.mjs`:

```js
test('a New email link opens on this installation only', () => {
  const link = 'memql-file://cluster.example/new-email/client-org/New%20email';
  assert.equal(requestedResource(handoff(link)), link);
  for (const invalid of [
    'memql-file://cluster.example/new/email?account=client-org&returnTo=email',
    link.replace('cluster.example', 'other.example'),
    link.replace('client-org', 'v1:accounts:account:client-org'),
    link + '#x',
  ]) assert.throws(() => requestedResource(handoff(invalid)));
});
```

- [ ] **Step 2: Run, verify failure**

Run: `cd editors/productivity && npm test` and `node --test editors/browser/test/handoff.test.mjs`
Expected: FAIL (the kind is refused).

- [ ] **Step 3: Implement.** In `Documents.read` and `save`, a `new-email` resource throws `Error("A New email link opens a page, not a file.")` before any connect. In `MemQLFiles`, `stat` and `readFile` check `resourceFrom(uri.toString()).kind === "new-email"` first: `{ type: vscode.FileType.File, ctime: 0, mtime: 0, size: 0, permissions: vscode.FilePermission.Readonly }` and `new Uint8Array()`; `writeFile` throws `vscode.FileSystemError.NoPermissions()`. The desktop URI handler, `memql.productivity.openFile` and the browser host keep calling `vscode.open`; the custom editor that answers it arrives in Task 9.

- [ ] **Step 4: Run**

Run: `cd editors/productivity && npm test && npm run compile && node --test ../browser/test/handoff.test.mjs`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add editors/productivity/src/documents.ts editors/productivity/src/extension.ts editors/productivity/test/documents.test.ts editors/browser/src/handoff.mjs editors/browser/test/handoff.test.mjs
git commit -F <message file>   # "feat(productivity): New email links open a page for an organization (#6032)"; body: the link contract for the OS plan's editorNewEmailURL
```

---

### Task 4: Shared page pieces: base styles, copy, preview and skeleton

Read first: `editors/productivity/src/markdownPage.ts` (the look being matched: buttons, `.primary`, `.views`, `.floating`, skeleton lines and sweep, the 1000 px and 600 px breakpoints); `editors/productivity/src/templatePreview.ts` `safeEmailHTML` (the sanitizer being superseded); engine plan Appendix A (the markup `previewHTML` receives).

**Files:**
- Create: `editors/productivity/src/pageBase.ts`
- Create: `editors/productivity/src/emailCopy.ts`
- Create: `editors/productivity/src/emailPreview.ts`
- Test: `editors/productivity/test/email-preview.test.ts`

**Interfaces:**
- Produces:

```ts
// pageBase.ts
export const ICONS: Readonly<Record<"design" | "source" | "desktop" | "mobile" | "plain" | "up" | "down" | "plus" | "trash" | "attach" | "mic" | "close" | "picture", string>>;  // inline 16 px SVG, stroke currentColor (markdownPage.ts's icon())
export function pageCSP(nonce: string): string;  // default-src 'none'; script-src 'nonce-<n>'; style-src 'unsafe-inline'; img-src data:; frame-src 'self'; form-action 'none'; base-uri 'none'
export function pageShell(options: { title: string; nonce: string; script: string; styles: string; body: string; bodyClass?: string }): string;
export const BASE_STYLES: string;  // reset, body type, buttons, .primary, .text-act, .icon-button, inputs/select/textarea, focus-visible, .views segmented control, .bar (sticky action bar), .notice, .subhead, details/summary, .switch (role=switch checkbox), .skeleton tone + sweep, .sr-only, reduced motion

// emailCopy.ts
export const EMAIL_COPY: { newEmail: {...}; editor: {...}; host: {...} };  // exact strings below
export function fill(template: string, values: Record<string, string | number>): string;  // "{name}" placeholders; an unknown placeholder stays visible

// emailPreview.ts
export const PREVIEW_CSP = "default-src 'none'; style-src 'unsafe-inline'; img-src data:; form-action 'none'; base-uri 'none'";
export function previewHTML(html: string, options: { blocks: boolean }): string;
export function skeletonHTML(blocks: readonly EmailBlock[]): string;
```

`EMAIL_COPY` (exact; `{x}` are placeholders):

```ts
newEmail: {
  title: "New email", organization: "Organization", layout: "Layout", builtIn: "Built in",
  name: "Name", namePlaceholder: "Autumn welcome", nameInvalid: "Use a name without slashes, up to 150 characters.",
  describe: "Describe it", optional: "Optional",
  describePlaceholder: "What should this email say? Include the audience, the offer and any links.",
  addReferences: "Add references or pictures", dictate: "Dictate the description", stopDictation: "Stop dictation",
  useInEmail: "Use in the email", removeReference: "Remove {name}",
  notConnected: "Not connected to {domain}", noOrganization: "No organization yet",
  noOrganizationLine: "Add an organization in MemQL OS to write emails.",
  chooseOrganization: "Choose an organization", chooseLayout: "Choose a layout", nameIt: "Name the email",
  ready: "Ready to write", writing: "Writing your email", failed: "Couldn’t write the email",
  layoutsFailed: "Couldn’t load the layouts", loading: "Loading layouts",
  connect: "Connect", start: "Start from the layout", write: "Write it", cancel: "Cancel", retry: "Retry",
},
editor: {
  design: "Design", source: "Source", desktop: "Desktop width", mobile: "Mobile width", plain: "Plain text",
  email: "Email", subject: "Subject", preheader: "Preheader", organization: "Organization", issues: "Issues", blocks: "Blocks",
  moveUp: "Move up", moveDown: "Move down", addAfter: "Add after", remove: "Remove block",
  words: "Words", label: "Label", link: "Link", description: "Description", picture: "Picture", noPicture: "No picture",
  chooseFromFiles: "Choose from Files", upload: "Upload", removePicture: "Remove",
  colour: "Colour", background: "Background", align: "Align", left: "Left", center: "Center", right: "Right",
  height: "Height", title: "Title", links: "Links", lines: "Lines", html: "HTML", network: "Network", addLink: "Add link",
  brand: "Same in every email", theme: "Theme", headingFont: "Heading font", bodyFont: "Body font",
  themeColours: { page: "Page", canvas: "Canvas", primary: "Primary", secondary: "Secondary", accent: "Accent", tint: "Tint", text: "Text", muted: "Muted", white: "White" },
  previewWith: "Preview with", sampleValue: "Sample value",
  sampleLabels: { displayName: "Name", email: "Email address", accountName: "Organization", campaignName: "Campaign" },
  htmlOnly: "Written in HTML", editHtml: "Edit HTML", plainOnly: "Plain text only",
  blockNames: { image: "Picture", heading: "Heading", text: "Text", highlight: "Highlight", button: "Button", divider: "Divider", spacer: "Space", section: "Section", columns: "Columns", social: "Social links", footer: "Footer", html: "HTML" },
  notConnected: "Not connected", notConnectedTo: "Not connected to {domain}", chooseOrganization: "Choose the organization",
  unreadable: "Can’t read this email", local: "On this computer",
  issuesOne: "Fix 1 issue to publish", issuesMany: "Fix {count} issues to publish",
  unsaved: "Unsaved changes", draft: "Draft", published: "Published to {organization}", changed: "Changes since publishing",
  publishing: "Publishing", savingLayout: "Saving the layout", savingCopy: "Saving to MemQL", layoutSaved: "Saved as a layout",
  connect: "Connect", openSource: "Open Source", saveToMemQL: "Save to MemQL", saveLayout: "Save as layout", publish: "Publish",
  layoutName: "Layout name", saveLayoutConfirm: "Save layout", cancel: "Cancel",
  renderFailed: "Couldn’t show this email.", invalidJson: "This email has invalid JSON. Open Source and correct its formatting.",
},
host: {
  connectFirst: "Connect to your cluster in MemQL before writing an email.",
  addOrganization: "Add an organization in MemQL OS before writing an email.",
  addReferences: "Add references", fromComputer: "From this computer", fromFiles: "From Files",
  chooseReferences: "Choose references", addLabel: "Add", recentFiles: "Recent files in MemQL",
  tooMany: "Choose up to 64 references.", tooLarge: "References must total 16 MiB or less.",
  unsupported: "Choose pictures, text, Markdown, HTML, CSS, JSON, CSV, SVG or ZIP files.",
  choosePicture: "Choose a picture", recentPictures: "Recent pictures in Files", noPictures: "No pictures in Files yet. Upload one instead.",
  uploadPicture: "Upload a picture", usePicture: "Use picture", pictureTooLarge: "Choose a picture of 1 MiB or less.",
  saveBeforePublish: "Save the email before publishing.", changedWhilePublishing: "The email changed while publishing. Publish again.",
  fixIssues: "Fix the issues shown in the editor, then publish.", noLayoutForHtml: "An email written in HTML can’t be a layout.",
  layoutNameInvalid: "Enter a layout name up to 200 characters.", editsNeedValidJson: "Fix the email in Source first.",
},
```

`previewHTML`: parse with `DOMParser`; keep `safeEmailHTML`'s tag and attribute allowlists (and its `img src` rule: only `data:image/(png|jpeg|gif);base64`); additionally keep `data-block` when `blocks` is true and the value matches `^[a-z0-9][a-z0-9-]{0,63}$`; keep the `<body>` element's allowed attributes; keep the renderer's `<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Transitional//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-transitional.dtd">` when the input starts with it (the same limited-quirks mode email clients use), else `<!doctype html>`; head = the `PREVIEW_CSP` meta plus the input's `<style>` elements.

`skeletonHTML` (no text from the email ever enters it; only numbers derived from block fields, clamped): `<div class="sk-email" aria-hidden="true">` wrapping one element per block: image `<div class="sk sk-picture" style="width:<w/6 %, 2 decimals>%;aspect-ratio:<w>/<h>">` (`h` = `height` or `round(w × 0.5)`), heading `<div class="sk sk-heading" style="height:<clamp(size × 0.8, 14, 40)>px">`, text `max(2, min(8, ceil(chars / 70)))` `<div class="sk sk-line">` with the last at 60 %, highlight `<div class="sk sk-band">`, button `<div class="sk sk-button" style="width:<w/6>%">` (`w` default 300), divider `<div class="sk-rule">`, spacer `<div class="sk-gap" style="height:<clamp(height / 2, 4, 60)>px">`, section `<div class="sk-section">…</div>`, columns `<div class="sk-columns">` with `<div style="flex:<width>">…</div>` per column, social `<div class="sk-social"><i></i><i></i><i></i></div>`, footer `<div class="sk-footer">` with three short centered lines, html two lines.

- [ ] **Step 1: Failing tests** (`test/email-preview.test.ts`, jsdom's `DOMParser` on `globalThis` as `template-preview.test.ts` does):

```ts
test("the preview keeps the email's layout and block marks, and nothing that runs or reaches out", () => {
  const window = new JSDOM("").window;
  Object.assign(globalThis, { DOMParser: window.DOMParser });
  const html = `<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Transitional//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-transitional.dtd"><html><head><meta http-equiv="refresh" content="0;url=https://outside.test"><style>@media (max-width:599px){.ecw{width:100%!important}}</style></head><body style="background-color:#eef0f3;margin:0"><table class="ecw"><tr><td data-block="headline" style="color:#1f5f8b"><a href="https://outside.test" onclick="alert(1)">Hi</a></td><td data-block="Bad Id">x</td></tr></table><script>alert(1)</script><img src="https://tracker.test/p.gif" alt="Pixel"><img src="data:image/png;base64,iVBORw0KGgo=" alt="Logo"></body></html>`;
  const out = previewHTML(html, { blocks: true });
  assert.ok(out.startsWith('<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Transitional//EN"'));
  const doc = new window.DOMParser().parseFromString(out, "text/html");
  assert.equal(doc.body.getAttribute("style"), "background-color:#eef0f3;margin:0");
  assert.equal(doc.querySelector('[data-block="headline"]')?.getAttribute("style"), "color:#1f5f8b");
  assert.equal(doc.querySelectorAll("[data-block]").length, 1, "only block ids survive as marks");
  assert.equal(doc.querySelectorAll("script,[href],[onclick],meta[http-equiv=refresh]").length, 0);
  assert.equal(doc.querySelectorAll("img").length, 1, "remote pictures never load");
  assert.match(doc.querySelector('meta[http-equiv="Content-Security-Policy"]')?.getAttribute("content") ?? "", /img-src data:/);
  assert.match(doc.querySelector("style")?.textContent ?? "", /\.ecw/);
  assert.ok(!previewHTML(html, { blocks: false }).includes("data-block"));
  window.close();
});
```

Also: `the skeleton follows the blocks and carries no words from the email` (a logo image 600×155, a section holding a text block whose text is `<img src=x onerror=alert(1)> ` repeated and a two-line highlight, a 305 px button, two columns, a footer: exactly two `class="sk sk-picture"`, the section contains lines and a band in order, the button carries `style="width:50.83%"`, there is one `sk-columns` and one `sk-footer`, and none of `onerror`, the button label, or footer words appear); `fill replaces placeholders and leaves unknown ones visible` (`fill("Published to {organization}", {organization: "Acme Co"})`, `fill("{x}", {})` is `{x}`).

- [ ] **Step 2: Run, verify failure** — `cd editors/productivity && npm test`; Expected: FAIL (modules missing).

- [ ] **Step 3: Implement** as specified. `BASE_STYLES` copies the Markdown page's values (button 34 px min height, 6 px radius, `.primary` colours, focus outline 2 px `--vscode-focusBorder` offset 2 px, `.views` 3 px padding 9 px radius, `.skeleton-line` tone `color-mix(in srgb, var(--vscode-editor-foreground) 10%, var(--vscode-editor-background))` and its 2.4 s sweep) so both email pages read as siblings of the Markdown editor. `.bar`: sticky bottom, 48 px, top hairline, editor background, padding `0 16px`, state left (13 px, `--vscode-descriptionForeground`, a 10 px spinner before busy words as Markdown's `.phase.busy`), acts right with 8 px gaps; `.text-act` is a link-coloured button with no border.

- [ ] **Step 4: Run** — `cd editors/productivity && npm test && npm run compile`; Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add editors/productivity/src/pageBase.ts editors/productivity/src/emailCopy.ts editors/productivity/src/emailPreview.ts editors/productivity/test/email-preview.test.ts
git commit -F <message file>   # "feat(productivity): shared email page pieces: styles, copy, preview and skeleton (#6032)"
```

---

### Task 5: The email editor page

Read first: spec 2.2; Rulings 6 to 9 and 13; `editors/productivity/src/markdownView.ts` (origin check on every message, `byId`/`textElement`, state restore, Ctrl/Cmd+Enter, focus handling); `editors/productivity/test/markdown-view.test.ts` (the jsdom harness pattern).

**Files:**
- Create: `editors/productivity/src/emailPage.ts`
- Create: `editors/productivity/src/emailView.ts`
- Modify: `editors/productivity/esbuild.js` (add `src/emailView.ts` to the IIFE entry points)
- Modify: `editors/productivity/esbuild.test.js` (also build `src/emailView.ts` to `dist-test/email-view.js`)
- Test: `editors/productivity/test/email-view.test.ts`

**Design note: the email editor.**

```
wide (>= 1000 px)
┌───────────────────────────────────────────────────────────────────────┐
│                    [ Design | Source ]                   [▭][▯][≡]    │ toolbar, sticky
├────────────────────────────────────────────┬──────────────────────────┤
│ email page colour fills the column         │ Email                    │
│      ┌──────── 600 px canvas ────────┐     │  Subject   [...........] │
│      │ LOGO                          │     │  Preheader [...........] │
│      │ ┌──── banner ─────┐           │     │ Issues (only with any)   │
│      │ ┌ tinted body ─────────────┐  │     │ Blocks                   │
│      │ │ Most busy mornings…   ◀──┼──┼─────│  Picture · Logo          │
│      │ │ ▬▬ highlight band ▬▬     │  │     │  Picture · Article banner│
│      │ └──────────────────────────┘  │     │  Section                 │
│      │      ( Ask our team )         │     │    Text · Most busy …  ▌ │
│      │ footer                        │     │ Text       [↑][↓][+][del]│
│      └───────────────────────────────┘     │  Words [textarea]        │
│                                            │  Colour [Text ▾] ■       │
│                                            │  Align  Left|Center|Right│
│                                            │  ◯ Same in every email   │
│                                            │ ▸ Theme                  │
│                                            │ ▸ Preview with           │
├────────────────────────────────────────────┴──────────────────────────┤
│ Draft                                          Save as layout [Publish]│ action bar, sticky
└───────────────────────────────────────────────────────────────────────┘
narrow (< 1000 px): the inspector becomes a 44vh bottom sheet above the action bar
(editorWidget background, top hairline); below 600 px the Design/Source buttons drop their icons.
```

- **Toolbar**: Design/Source segmented control centered (Markdown's `.views`, `aria-pressed`); at the right a three-way width control (`role="radiogroup"`, icon buttons with labels Desktop width, Mobile width, Plain text). With an empty HTML body only Plain text is offered (the others are absent) and the preview shows the text.
- **Preview**: two `iframe sandbox="allow-same-origin"` elements, `srcdoc` = `previewHTML(render.html, {blocks: true})` plus an editor style element (`[data-block]{cursor:pointer}`, hover `outline:1px dashed <focus>`, selected `outline:2px solid <focus>`; `<focus>` is the literal value of `--vscode-focusBorder` read with `getComputedStyle`). The back frame loads, gets its height from `documentElement.scrollHeight`, then swaps to front. Desktop frame width `max(column, 640)` px scaled by `min(1, column / 640)`; Mobile 375 px centered with a hairline; Plain text a `<pre>` in the editor monospace, max 72ch. A click inside the frame (capture listener on its document) prevents default and selects `closest("[data-block]")`. Before the first render, and while not connected, `#skeleton` shows `skeletonHTML(blocks)` (sweep only while loading); for a custom email offline it shows the plain text.
- **Inspector** (no boxes; 14 px subheads): Email (Subject, Preheader); Organization (only when the host sends `choices`); Issues (only with issues: each message is a link-styled button that selects its block); Blocks (outline, `role="tree"`, `treeitem` rows with `aria-level` and `aria-selected`, 16 px indent per level, a 6 px `--vscode-errorForeground` dot on rows with an issue, selected row `--vscode-list-inactiveSelectionBackground` with `box-shadow: inset 2px 0 var(--vscode-focusBorder)`); the selected block's group titled by its block name with icon acts Move up, Move down (each absent when `canMove` is false), Add after (menu of Heading, Text, Highlight, Button, Picture, Divider, Space), Remove block; then its fields per `EDITABLE_FIELDS` (Words, Label, Link with inline validation using `validLink`, Picture with Choose from Files, Upload and Remove as text acts and "No picture" when empty, Description, Colour select of theme tokens with a 12 px swatch of the theme's value, Background, Align segmented, Height, Title, Links rows with Add link, Lines, HTML), and the switch "Same in every email" last; then the Theme disclosure (eight `input type="color"` with labels, Heading font, Body font) and the Preview with disclosure (one input per `sampleKeys` key, labels from `sampleLabels` or the field key, placeholder Sample value; the disclosure is absent when the email uses no merge tags). Custom mode replaces Blocks and Theme with one line "Written in HTML" and the text act Edit HTML.
- **Action bar**: state words from the host (`role="status"`, polite), then acts in the host's order with Rulings 4's styles. Save as layout swaps the bar's content for a Layout name field (prefilled), Cancel (text) and Save layout (button); Enter saves, Escape restores the bar. A failed act shows one notice line above the bar (`role="alert"`, `renderProblem` with Details), cleared by the next act.
- **Keyboard**: Arrow Up/Down move through the outline, Alt+Arrow moves the block (when legal), Enter focuses the first field, Escape closes the Add menu; every control has a visible focus outline.

**Message protocol** (exact; every received message is ignored unless `event.origin === window.origin`):

```ts
type Host =
  | { type: "document"; version: number; email: EmailDocument | null; invalid?: string }
  | { type: "render"; seq: number; html: string; text: string; subject: string; issues: { code: string; block?: string; message: string }[] }
  | { type: "renderFailed"; seq: number; problem: Problem }
  | { type: "context"; connected: boolean; organization?: { id: string; name: string }; choices: { id: string; name: string }[] }
  | { type: "bar"; state: string; acts: BarAct[]; layoutName?: string }
  | { type: "restore"; state: ViewState }
  | { type: "select"; block: string }
  | { type: "problem"; problem: Problem };
type View =
  | { type: "ready" } | { type: "edit"; op: EmailEdit } | { type: "viewState"; state: ViewState }
  | { type: "act"; act: BarAct } | { type: "saveLayout"; name: string } | { type: "organization"; id: string }
  | { type: "picture"; block: string; action: "choose" | "upload" | "remove" } | { type: "mode"; mode: "source" | "split" }
  | { type: "shown"; version: number; subject: string; state: string; blocks: number }
  | { type: "copyProblemReference"; reference: string };
interface ViewState { width: "desktop" | "mobile" | "plain"; selected?: string; themeOpen?: boolean; samplesOpen?: boolean; samples?: Record<string, string> }
type BarAct = "connect" | "openSource" | "saveToMemQL" | "saveLayout" | "publish";
```

- [ ] **Step 1: Failing tests** (`test/email-view.test.ts`):

```ts
function fixture() {
  const dom = new JSDOM(emailPage({ title: "Welcome.email.json", script: "", nonce: "test" }), { runScripts: "outside-only" });
  const messages: any[] = [];
  Object.assign(dom.window, { acquireVsCodeApi: () => ({ postMessage: (m: any) => messages.push(m), getState: () => undefined, setState: () => {} }) });
  dom.window.eval(readFileSync("dist-test/email-view.js", "utf8"));
  const doc = dom.window.document, el = (id: string) => doc.getElementById(id)!;
  const send = (data: any) => dom.window.dispatchEvent(new dom.window.MessageEvent("message", { data, origin: dom.window.origin }));
  // jsdom ignores srcdoc: write the pending frame's srcdoc into it, then fire its load event.
  const loadPreview = () => {
    const frame = doc.querySelector<HTMLIFrameElement>('iframe[data-pending="1"]')!;
    const d = frame.contentDocument!; d.open(); d.write(frame.getAttribute("srcdoc")!); d.close();
    frame.dispatchEvent(new dom.window.Event("load"));
    return frame;
  };
  return { dom, doc, el, send, loadPreview, messages };
}
const article = { subject: "Hello", theme: {/* neutral theme */}, blocks: [
  { id: "headline", type: "heading", text: "Hi" },
  { id: "body", type: "section", blocks: [{ id: "intro", type: "text", text: "Hello {{displayName}}" }] },
  { id: "cta", type: "button", label: "Go", href: "https://example.com" },
] };
const rendered = `<html><body><table><tr><td data-block="headline">Hi</td></tr><tr><td data-block="body"><table><tr><td data-block="intro">Hello</td></tr></table></td></tr><tr><td data-block="cta"><a href="https://example.com">Go</a></td></tr></table></body></html>`;

test("clicking a block in the preview selects it, shows its fields, and never follows its link", () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  f.send({ type: "render", seq: 1, html: rendered, text: "Hi", subject: "Hello", issues: [] });
  const frame = f.loadPreview();
  const click = new f.dom.window.MouseEvent("click", { bubbles: true, cancelable: true });
  frame.contentDocument!.querySelector('[data-block="cta"]')!.dispatchEvent(click);
  assert.equal(click.defaultPrevented, true);
  assert.equal(f.el("block-title").textContent, "Button");
  assert.equal((f.el("field-label") as HTMLInputElement).value, "Go");
  assert.equal(f.doc.querySelector('[role="treeitem"][aria-selected="true"]')?.getAttribute("data-block"), "cta");
  f.dom.window.close();
});
```

Also, one `test(...)` each:
- `the outline lists the blocks and the first render replaces the skeleton` (`#skeleton` visible after `document`, hidden after the first frame load; four treeitems with levels 1, 1, 2, 1).
- `typing sends one edit after a pause or on leaving the field` (select `headline`, set the Words field to `W`, `We`, `Welcome` with `input` events, then `blur`: exactly one `{type: "edit", op: {kind: "field", block: "headline", field: "text", value: "Welcome"}}`).
- `a document update does not overwrite the field being typed in` (focus Subject, type `Typing`, send a document whose subject is `Old`: the input keeps `Typing`; the outline updates).
- `move, add and remove are offered only where legal` (first top-level block: no Move up; last: no Move down; Add after menu lists the seven types; choosing Text posts the `add` op; a later `select` message moves the selection to the new id).
- `issues mark their blocks and select them when clicked`.
- `the action bar shows the host's state and acts, with the one button last` (`bar` with `["saveLayout", "publish"]`: texts `Save as layout`, `Publish`, only the last has `.primary`; clicking Publish posts `{type: "act", act: "publish"}`; Save as layout opens the inline field prefilled with `layoutName`, Enter posts `{type: "saveLayout", name}`, Escape restores the bar).
- `custom-mode emails show the rendered email without an outline or theme` (Edit HTML posts `{type: "mode", mode: "split"}`).
- `Preview with lists only the merge tags used and keeps the values in the page state` (an input posts `viewState` with `samples` after the pause; absent when no tags).
- `the width choice switches the preview and is restored` (Mobile adds `.mobile` to `#preview` and posts `viewState`; a `restore` with `width: "plain"` shows the `<pre>`).
- `a render failure keeps the last preview dimmed with the problem` (`#preview` gets `.stale`, `#preview-notice` shows the message and a Details disclosure with the reference).
- `messages from another origin are ignored`.
- `arrow keys move through the outline and Alt+Arrow moves a block`.
- `theme colours and fonts send theme edits`.
- `picture acts post the block and the action, and Remove is absent without a picture`.
- `every document message reports what was shown` (`shown` with version, subject, the bar's state and the block count: the host suite's hook).

- [ ] **Step 2: Run, verify failure** — `cd editors/productivity && npm test`; Expected: FAIL.

- [ ] **Step 3: Implement** `emailPage(options: { title: string; script: string; nonce: string }): string` with `pageShell` and the static DOM (ids used by the tests: `view-design`, `view-source`, `width-desktop`, `width-mobile`, `width-plain`, `preview`, `skeleton`, `plain`, `preview-notice`, `subject`, `preheader`, `organization-row`, `organization`, `issues`, `outline`, `block`, `block-title`, `move-up`, `move-down`, `add-after`, `add-menu`, `remove-block`, field ids `field-<name>`, `theme`, `samples`, `custom-note`, `bar`, `bar-state`, `bar-acts`, `bar-form`, `layout-name`, `layout-cancel`, `layout-save`, `bar-notice`; the two preview frames are `frame-a` and `frame-b`, and the one loading carries `data-pending="1"` until its load), and `emailView.ts` to the protocol. All visible strings come from `EMAIL_COPY`. Selection, width and disclosure changes post `viewState`; nothing else is sent until the person acts.

- [ ] **Step 4: Run** — `cd editors/productivity && npm test && npm run compile`; Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add editors/productivity/src/emailPage.ts editors/productivity/src/emailView.ts editors/productivity/esbuild.js editors/productivity/esbuild.test.js editors/productivity/test/email-view.test.ts
git commit -F <message file>   # "feat(productivity): the email Design page: live preview, outline and block inspector (#6032)"
```

---

### Task 6: Organization, publishing and layouts

Read first: Rulings 5 and 9 to 12; engine plan Rulings 14 to 16 and Tasks 7 and 9 (`campaignSaveTemplate` document mode, `templateForSource`, `campaignSaveLayout`, `emailLayouts` rows, receipts); `editors/productivity/src/templatePublish.ts` and `test/templatePublish.test.ts` (the behaviours carried over: single flight, edit during publish).

**Files:**
- Create: `editors/productivity/src/emailPublish.ts`
- Test: `editors/productivity/test/email-publish.test.ts`

**Interfaces:**
- Consumes: `buildClientAccountsAll`, `buildCompositionForOutputFile`, `buildSetArtifactAccounts` (exist); `buildTemplateForSource`, `buildCampaignSaveTemplate` (with `sourceArtifactId`, optional `templateId`), `buildEmailLayouts`, `buildCampaignSaveLayout` (engine Tasks 7 and 9).
- Produces (exact):

```ts
export interface Organization { id: string; name: string }
export type EmailTarget =
  | { kind: "file"; domain: string; artifactId: string; fileId: string; name: string; labels: string[] }
  | { kind: "template"; domain: string; templateId: string; name: string; accountId: string; status: string; revision: string }
  | { kind: "local"; name: string };
export interface PublishPorts {
  execute(name: string, call: string): Promise<Record<string, unknown>[]>;
  state: { get<T>(key: string): T | undefined; update(key: string, value: unknown): PromiseLike<void> };
}
export interface TemplateState { templateId: string; status: string; revision: string; document?: Record<string, unknown> }
export function organizations(ports: PublishPorts): Promise<Organization[]>;     // clientAccountsAll, status active, by name
export function resolveOrganization(target: EmailTarget, ports: PublishPorts, remembered?: string): Promise<{ organization?: Organization; choices: Organization[] }>;
export function chooseOrganization(target: EmailTarget, organization: Organization, ports: PublishPorts): Promise<"labelled" | "remembered">;
export function templateFor(target: EmailTarget, organization: Organization, ports: PublishPorts): Promise<TemplateState | undefined>;
export interface BarInput {
  connected: boolean; domain?: string; invalid: boolean; local: boolean; organization?: Organization;
  mode: "layout" | "custom"; dirty: boolean; issues: number; template?: TemplateState; unchanged: boolean;
  busy?: "publish" | "layout" | "copy"; layoutSaved?: boolean;
}
export function editorBar(input: BarInput): { state: string; acts: BarAct[] };
export class EmailPublisher {
  constructor(ports: PublishPorts);
  // `target` is read after `save()`: saving a template resource moves its revision (Documents.save updates it).
  publish(input: { target: () => EmailTarget; organization: Organization; email: () => EmailDocument; save: () => PromiseLike<boolean>; stillCurrent: () => boolean }): Promise<TemplateState>;
  saveLayout(input: { target: EmailTarget; organization: Organization; email: EmailDocument; name: string; key: string }): Promise<{ layoutId: string; revision: string }>;
}
```

`editorBar` (exact; first match wins; `{n}`/`{organization}`/`{domain}` filled from `EMAIL_COPY.editor`):

| # | Condition | State | Acts |
|---|---|---|---|
| 1 | `busy` | Publishing / Saving the layout / Saving to MemQL | none |
| 2 | not connected | Not connected to {domain} (or Not connected for a local file) | connect |
| 3 | `invalid` | Can’t read this email | openSource |
| 4 | no organization | Choose the organization | none |
| 5 | `local` | On this computer | saveToMemQL |
| 6 | `issues > 0` | Fix 1 issue to publish / Fix {n} issues to publish | saveLayout (layout mode) |
| 7 | `dirty` | Unsaved changes | saveLayout (layout mode), publish |
| 8 | template ready and `unchanged` | Published to {organization} | saveLayout (layout mode) |
| 9 | template ready, changed | Changes since publishing | saveLayout (layout mode), publish |
| 10 | otherwise (no template, draft, archived) | Draft | saveLayout (layout mode), publish |

`layoutSaved` replaces the state of rows 6 to 10 with "Saved as a layout" (the host clears it after 4 s). For a template target, `unchanged` is `!dirty` (the template is the content).

`publish`: single flight per target; `save()` first (the caller saves a dirty buffer and answers false when it could not: `UserInputError(host.saveBeforePublish)`), then reads `target()` and `email()`; for a file: `templateForSource` then save (`name` = file name without `.email.json`, `content` = the email, `sourceArtifactId`, `expectedRevision` = the found row's `createdAt` or `""`, no `templateId`), refuse with `host.fixIssues` when the receipt's `issues` is non-empty, check `stillCurrent()` (`host.changedWhilePublishing`), then publish (`templateId` and `expectedRevision` from the receipt); for a template: publish with its `templateId`, `accountId`, `name`, opened `revision`, then update the target's `status` and `revision`. No `workspaceState` key is written. `saveLayout`: refuses custom mode (`host.noLayoutForHtml`) and names over 200 characters or with control characters (`host.layoutNameInvalid`); finds the organization's `emailLayouts` row whose `sourceArtifactId` equals the file's artifact id (files only) and updates it (`layoutId: row.layoutId`, `expectedRevision: row.revision`, `description: row.description`), otherwise creates with `layoutId` = `state.get("memql.emailLayoutPending:" + key)` or a new UUID stored there before the call, cleared after the receipt.

- [ ] **Step 1: Failing tests** (`test/email-publish.test.ts`; a `ports(rows)` helper returns `{calls, state, ports}`: it records `{name, call}`, answers from per-builtin functions and keeps `state` in a `Map`; `welcome` is a small layout email built with Task 2's `layout()` helper):
- `a file the Materializer wrote knows its organization without asking` (`compositionForOutputFile` gives `accountIds: ["client-org"]`; two active organizations exist; the answer is `{organization: {id: "client-org", name: "Client"}, choices: []}`).
- `a template knows its organization`.
- `one label or one organization is enough; otherwise the person chooses` (several labels or none, several organizations: `choices` lists the active ones by name and `organization` is undefined; a `remembered` id among them resolves).
- `choosing labels an unlabelled Library file once` (`setArtifactAccounts` with `artifactId: "a1"` and `accountIds: ["client-org"]`; a labelled file or a local file returns `"remembered"` and writes nothing).
- `the action bar follows the email's state` (one assertion per table row, including custom mode without Save as layout and at most three acts everywhere).
- `first publish saves then publishes the exact revision, keyed by the file`:

```ts
test("first publish saves then publishes the exact revision, keyed by the file", async () => {
  const f = ports({
    templateForSource: () => [],
    campaignSaveTemplate: call => /action: "publish"/.test(call)
      ? [{ templateId: "t1", revision: "2026-10-11T10:00:01Z", status: "ready", saved: true, issues: [] }]
      : [{ templateId: "t1", revision: "2026-10-11T10:00:00Z", status: "draft", saved: true, issues: [] }],
  });
  const target = { kind: "file", domain: "client.example", artifactId: "a1", fileId: "f1", name: "Welcome.email.json", labels: [] } as const;
  await new EmailPublisher(f.ports).publish({ target: () => target, organization: { id: "client-org", name: "Client" }, email: () => welcome, save: async () => true, stillCurrent: () => true });
  assert.deepEqual(f.calls.map(c => c.name), ["templateForSource", "campaignSaveTemplate", "campaignSaveTemplate"]);
  assert.match(f.calls[1].call, /action: "save"/);
  assert.match(f.calls[1].call, /sourceArtifactId: "a1"/);
  assert.match(f.calls[1].call, /expectedRevision: ""/);
  assert.doesNotMatch(f.calls[1].call, /templateId:/);
  assert.match(f.calls[1].call, /name: "Welcome"/);
  assert.match(f.calls[2].call, /templateId: "t1"/);
  assert.match(f.calls[2].call, /expectedRevision: "2026-10-11T10:00:00Z"/);
  assert.ok([...f.state.keys()].every(key => !key.startsWith("memql.campaignPublication")), "no local publication mapping");
});
```

- `republishing updates the template found for the file` (the found row's `createdAt` is the save's `expectedRevision`).
- `a lost response is recovered without a second template` (the first save throws after the fake records a template; the retry reads it through `templateForSource` and saves with its revision; only one save ever carries `expectedRevision: ""`).
- `a double click publishes once`.
- `an edit while publishing stops before the publish call`.
- `issues from the save stop the publish`.
- `a template resource publishes with its opened revision` (no `templateForSource` call; the target's status becomes `ready`).
- `save as layout creates once with a stable id and updates the layout saved from the same file` (a lost receipt retries with the stored `layoutId`; with an `emailLayouts` row `{id: "org:l1", layoutId: "l1", sourceArtifactId: "a1", revision: "r1", name: "Monthly", kind: "organization"}` the call carries `layoutId: "l1"` and `expectedRevision: "r1"`; the client never builds `org:`).
- `an email written in HTML cannot be a layout`.

- [ ] **Step 2: Run, verify failure** — `cd editors/productivity && npm test`; Expected: FAIL.

- [ ] **Step 3: Implement** as specified.

- [ ] **Step 4: Run** — `cd editors/productivity && npm test && npm run compile`; Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add editors/productivity/src/emailPublish.ts editors/productivity/test/email-publish.test.ts
git commit -F <message file>   # "feat(productivity): publish emails and save layouts by file, with no local mapping (#6032)"; body: starts consuming templateForSource, campaignSaveTemplate document mode, emailLayouts, campaignSaveLayout
```

---

### Task 7: The email editor host replaces the template editor

Read first: `editors/productivity/src/templateEditor.ts` (being replaced), `editors/productivity/src/markdownEditor.ts` `show` (the Source/Split pattern, including why a source tab is revealed, not closed), `editors/productivity/test/markdown-refresh.test.ts` (unit-testing a provider with a fake panel), `editors/productivity/test/host.ts`.

**Files:**
- Create: `editors/productivity/src/emailEditor.ts`
- Modify: `editors/productivity/src/extension.ts` (register `EmailEditor` for `memql.productivity.email`; commands `memql.productivity.email.design`, `.source`, `.split`; remove `memql.productivity.template.*`, `previewTemplate`, `newTemplate`; test hook `emailReady` replaces `templateReady`)
- Modify: `editors/productivity/package.json` (the three commands, category `MemQL`, icons `$(preview)`, `$(code)`, `$(open-preview)`; `editor/title` entries for `resourceFilename =~ /\.email\.json$/i`: design `navigation@1`, source `navigation@2`, split `2_email@3`; remove the old template commands and menus)
- Modify: `editors/productivity/esbuild.js` (remove `src/templateView.ts`)
- Modify: `editors/productivity/test/support/vscode.ts` (`ViewColumn`, `Range`, `Position`, `WorkspaceEdit`, `workspace.applyEdit`, `window.tabGroups`, `TabInputCustom`)
- Modify: `editors/productivity/test/onePathCreate.test.ts` (front-door Task 7's guard: add `emailEditor.ts` to `ALLOWED` with the reason "uploads a picture the person picked, or the local email they save to MemQL", since this task introduces its `createFile(` calls)
- Modify: `editors/productivity/test/host.ts`; `editors/productivity/test/documents.test.ts` (drop the `templates.js` import: the case "email template files validate independently of a cluster" is covered by Task 2's tests and goes; "campaign template saves use the opened client and exact revision" uses a literal custom email and `readEmail`); `editors/browser/test/markdown-controls.test.mjs` (one more test for the email controls)
- Delete: `editors/productivity/src/templateEditor.ts`, `src/templateView.ts`, `src/templatePreview.ts`, `src/templatePublish.ts`, `src/templates.ts`, `test/template-preview.test.ts`, `test/templatePublish.test.ts`
- Test: `editors/productivity/test/email-editor.test.ts`

**Interfaces:**
- Consumes: Tasks 2 and 4 to 6; `buildEmailRender` (engine Task 8), `buildLibraryFilesForOwner`, `buildLibraryArtifactForFile`; `Documents.createFile`; `refreshQueue`.
- Produces:

```ts
export class EmailEditor implements vscode.CustomTextEditorProvider {
  constructor(context: vscode.ExtensionContext, api: EditorConnectionAPI, files: Documents, load: (uri: vscode.Uri) => Promise<OpenDocument>);
  resolveCustomTextEditor(document: vscode.TextDocument, panel: vscode.WebviewPanel): Promise<void>;
  show(mode: "design" | "source" | "split", uri?: vscode.Uri): Promise<void>;
  whenShown(uri: vscode.Uri, version?: number): Promise<{ subject: string; state: string; blocks: number }>;  // test hook
}
```

Behaviour:
- **Target**: `memql-file` artifacts become `file` targets (artifact id, `sourceId` as file id, `accountIds` as labels, name from the resource); `memql-file` templates become `template` targets from `OpenDocument.template`; anything else is `local`.
- **Connection**: never connects on its own. Connect calls `api.connect(domain)` for MemQL files and `memql.clusters.select` for local files. `api.onDidChange` re-resolves the organization and re-renders.
- **Render**: on every document change and on `viewState` sample changes, wait 300 ms of quiet, then one `emailRender({document, accountId, sample})` at a time through `refreshQueue`; a result older than the newest request is dropped (sequence numbers). `sample` is `{displayName, email, accountName, campaignName, fields: {<key>: value}}` built from the stored values. The render's `issues` feed the bar.
- **Edits**: an `edit` reads the current text; `readEmail` failure posts `problem` with `host.editsNeedValidJson`; otherwise `editEmail`, `writeEmail`, `minimalEdit`, and one `WorkspaceEdit.replace` over that span. After an `add`, it posts `select` with `addedBlockId`.
- **Pictures**: choose shows a quick pick (`host.choosePicture`, placeholder `host.recentPictures`) of `libraryFilesForOwner` rows whose MIME is PNG, JPEG or GIF, resolves `libraryArtifactForFile`, and sets `src` to `artifact:<id>` (and `alt` from the file name when empty); none gives the toast `host.noPictures`. Upload uses `showOpenDialog` (`host.uploadPicture`, label `host.usePicture`, png/jpg/jpeg/gif), refuses over 1 MiB (`host.pictureTooLarge`), calls `files.createFile` and uses the returned link's artifact id (`resourceFrom(uri).id`). Remove sets `src` to `""`.
- **Acts**: publish and save-as-layout go through `EmailPublisher` (busy state while running); Save to MemQL saves the local file, uploads it with `files.createFile(lease, basename, "application/json", bytes)`, labels it when an organization was chosen, opens it with `vscode.openWith(uri, "memql.productivity.email", {viewColumn: panel.viewColumn, preview: false})`, and closes the local tab (`tabGroups.close` on its `TabInputCustom`). Publishing never opens anything.
- **State**: `workspaceState["memql.emailView:" + uri]` holds `ViewState` (restored on `ready`); the organization choice for local or multi-label files is kept there too.
- **Modes**: `show("source")` shows the text document in the panel's column and disposes the panel; `"split"` shows Source beside the Design panel and reuses an existing pair; `"design"` opens the custom editor in the source's column (mirror `MarkdownEditor.show`).

- [ ] **Step 1: Failing tests** (`test/email-editor.test.ts`, with a fake panel, a fake document and fake ports, as `markdown-refresh.test.ts` does):
- `the preview renders through emailRender with the organization and samples, once per pause` (three quick document changes make one `emailRender` call carrying the last document, `accountId: "client-org"` and the stored `fields`).
- `an inspector edit becomes one minimal text edit` (the `WorkspaceEdit` replaces only the changed span; undo-friendly).
- `edits are refused while the text is not valid JSON` (a `problem` message, no `applyEdit`).
- `a stale render result is dropped`.
- `pictures: choose from Files sets an artifact source, upload goes through the upload path, remove leaves the slot`.
- `publishing stays in the same editor` (no `vscode.openWith` or `showTextDocument` during publish; the bar goes Publishing, then Published to Client).
- `Save to MemQL uploads a local email, labels it and reopens it in place`.
- `the editor connects only when asked` (no `connect` until the `connect` act).
- `the page state, including Preview with values, is kept per email and restored on ready`.

Host suite (`test/host.ts`): replace the template part with: the commands `memql.productivity.email.design`, `.source`, `.split` are registered; a local `host.email.json` custom-mode email opens with `vscode.openWith(uri, "memql.productivity.email")` and `emailReady(uri)` gives subject `Thanks for subscribing` and state `Not connected`; Split shows the same `TextDocument`; a subject edit through `WorkspaceEdit` reaches `emailReady(uri, version)` as `Updated live preview`; Source keeps the unsaved buffer; Design after save renders again; a local `host-layout.email.json` layout document reports `blocks: 2`. Markdown and PDF parts are unchanged.

`editors/browser/test/markdown-controls.test.mjs`: `Email view buttons stay in the editor header for email files only` (the three entries use `navigation@N` or `2_email@N`, their icons are codicons the host supplies, they are visible for `welcome.email.json` and `Welcome.EMAIL.JSON` and absent for `review.md`).

- [ ] **Step 2: Run, verify failure**

Run: `cd editors/productivity && npm test`
Expected: FAIL (`emailEditor.js` missing).

- [ ] **Step 3: Implement**, wire `extension.ts`, update the manifest and delete the replaced files. `templateExamples.ts` stays until Task 9; it opens its result with `memql.productivity.email`, which is now this editor.

- [ ] **Step 4: Run everything this touched**

```bash
cd editors/productivity && npm test && npm run compile && cd ../..
node editors/productivity/test/runner.cjs && node editors/productivity/test/runner.cjs --desktop
MEMQL_VSCODE_VERSION=1.91.0 node editors/productivity/test/runner.cjs --desktop
npm --prefix editors/browser test
```

Expected: PASS everywhere, and both hosts print the suite's PASS line.

- [ ] **Step 5: Commit**

```bash
git rm editors/productivity/src/templateEditor.ts editors/productivity/src/templateView.ts editors/productivity/src/templatePreview.ts editors/productivity/src/templatePublish.ts editors/productivity/src/templates.ts editors/productivity/test/template-preview.test.ts editors/productivity/test/templatePublish.test.ts
git add editors/productivity/src/emailEditor.ts editors/productivity/src/extension.ts editors/productivity/package.json editors/productivity/esbuild.js editors/productivity/test/support/vscode.ts editors/productivity/test/onePathCreate.test.ts editors/productivity/test/host.ts editors/productivity/test/documents.test.ts editors/productivity/test/email-editor.test.ts editors/browser/test/markdown-controls.test.mjs
git commit -F <message file>   # "feat(productivity): the email editor: Design, Source and Split on one buffer, Publish in place (#6032)"; body: starts consuming emailRender; the workspaceState publication mapping is gone
```

---

### Task 8: The New email page

Read first: spec 2.1; Rulings 1, 3, 4 and 14; `editors/productivity/src/markdownPage.ts` empty-document section and composer (the look of "Create your document": 24 px title, textarea, attach and dictate tools); `editors/productivity/src/markdownView.ts` `dictate` and `dictationStatus` (the dictation states and copy).

**Files:**
- Create: `editors/productivity/src/newEmailPage.ts`
- Create: `editors/productivity/src/newEmailView.ts`
- Modify: `editors/productivity/esbuild.js` (add `src/newEmailView.ts`), `editors/productivity/esbuild.test.js` (build `dist-test/new-email-view.js`)
- Test: `editors/productivity/test/new-email-view.test.ts`

**Design note: New email.**

```
┌──────────────────────────────────────────────────────────────┐
│ New email                                                    │ head: 24 px title; meta is the organization
│ Acme Co                                                      │ when there is only one (a quiet line)
│                                                              │
│ Organization  [ Acme Co              ▾ ]                     │ only with more than one organization
│                                                              │
│ Layout                                                       │
│  Acme Co                                                     │ subheads only when both groups exist
│  ┌──────┐ ┌──────┐                                           │
│  │ thumb│ │ thumb│   radiogroup of cards: scaled renders     │
│  └──────┘ └──────┘   168 × 218, 8 px radius, hairline;       │
│  Monthly   Welcome   the chosen one has a 2 px focus ring    │
│  Built in                                                    │
│  ┌──────┐ ┌──────┐ ┌──────┐ ┌──────┐                         │
│  └──────┘ └──────┘ └──────┘ └──────┘                         │
│  Confirm…  Letter   Article  Spotlight                       │
│  name 13 px 600; description 12 px quiet, two lines          │
│                                                              │
│ Name      [ Autumn welcome                 ]                 │
│                                                              │
│ Describe it  Optional                                        │
│ ┌──────────────────────────────────────────────────────────┐ │
│ │ What should this email say? Include the audience, …      │ │ textarea, min 120 px
│ └──────────────────────────────────────────────────────────┘ │
│ [attach] [mic]  brief.md ×   logo.png  ◯ Use in the email ×   │ tools, then reference chips
├──────────────────────────────────────────────────────────────┤
│ Ready to write                Start from the layout [Write it]│ action bar
└──────────────────────────────────────────────────────────────┘
writing: the form is replaced by the chosen layout's skeleton (centered 600 px, sweep),
screen-reader status "Writing your email"; bar: Writing your email · Cancel
```

- Single column, max 880 px, left aligned, 24 px between groups, no boxes. Cards: grid `repeat(auto-fill, minmax(168px, 1fr))`, 16 px gap; the thumbnail is a `sandbox` (no tokens) `srcdoc` iframe of `previewHTML(html, {blocks: false})` at 600 × 780 scaled 0.28, `aria-hidden`, pointer events off; until it arrives, or when its render fails, the card shows `skeletonHTML` of that layout. Cards are `role="radio"` in a `role="radiogroup"` labelled Layout, with roving tabindex and arrow keys.
- Name: input with inline validation (`nameInvalid`) only when invalid. Describe it: textarea (max 8000 characters), Ctrl/Cmd+Enter writes; under it the attach icon button (`addReferences`) and dictate icon button (only when the host can dictate; label toggles to `stopDictation`; status lines as in Markdown), then chips: a file icon, the name, for pictures and ZIPs a compact switch `Use in the email` (default off), a remove icon button (`removeReference`); an uploading chip shows a 10 px spinner.
- States and acts (first match wins): not connected (Not connected to {domain}: Connect); loading (no words, screen-reader Loading layouts, skeleton cards); layouts failed (Couldn’t load the layouts: Retry, with the problem notice); no organization (No organization yet, notice `noOrganizationLine`); writing (Writing your email: Cancel); no organization chosen (Choose an organization); no layout (Choose a layout); no valid name (Name the email); otherwise Ready to write (Start from the layout as the button, or Start from the layout and Write it when there is a description). After a failure the state is Couldn’t write the email with the same acts until the next change.

**Message protocol** (exact; origin-checked):

```ts
type Host =
  | { type: "setup"; connected: boolean; domain: string; organizations: { id: string; name: string }[]; organizationId?: string;
      layouts: { id: string; name: string; description: string; kind: "builtin" | "organization"; document: LayoutEmail }[]; dictation: boolean }
  | { type: "layoutsFailed"; problem: Problem }
  | { type: "thumbnail"; id: string; html: string }
  | { type: "references"; items: { label: string; kind: "picture" | "bundle" | "file"; include: boolean; uploading: boolean }[]; problem?: Problem }
  | { type: "writing"; layoutId: string } | { type: "failed"; problem: Problem } | { type: "idle" }
  | { type: "restore"; draft: Draft }
  | { type: "dictation"; phase: "starting" | "listening" | "transcribing" | "idle"; text?: string; final?: boolean; error?: string; reference?: string };
type Draft = { organizationId?: string; layoutId?: string; name?: string; description?: string };  // references are kept host-side
type View =
  | { type: "ready" } | { type: "organization"; id: string } | { type: "draft"; draft: Draft }
  | { type: "attach" } | { type: "removeReference"; index: number } | { type: "include"; index: number; on: boolean }
  | { type: "start"; layoutId: string; name: string } | { type: "write"; layoutId: string; name: string; description: string }
  | { type: "cancel" } | { type: "connect" } | { type: "retry" }
  | { type: "dictationStart" } | { type: "dictationStop" } | { type: "dictationCancel" }
  | { type: "shown"; state: string } | { type: "copyProblemReference"; reference: string };
```

- [ ] **Step 1: Failing tests** (`test/new-email-view.test.ts`, same harness shape as Task 5):
- `choosing a layout and a name makes it ready; a description adds Write it` (after `setup` with one organization and four built-ins: state Choose a layout; clicking Article: Name the email; typing a name: Ready to write with one button Start from the layout; typing a description: Start from the layout as a text act, then the Write it button).
- `Write it posts the whole request and the page shows the layout's shape while writing` (`{type: "write", layoutId: "builtin:article", name: "Autumn welcome", description}`; after `writing`, the form is hidden, `.sk-email` is shown, the bar reads Writing your email with Cancel; Cancel posts `cancel`; `idle` restores the form with everything typed).
- `a failure keeps the form filled and shows the problem with its reference`.
- `thumbnails replace each card's skeleton as they arrive`.
- `references show as chips; pictures and ZIPs have a switch; removing posts the index`.
- `the Organization select appears only with more than one organization and posts a change`.
- `not connected shows only Connect`.
- `arrow keys move the layout choice`.
- `the draft is posted for keeping and restored on ready`.
- `dictation appears only where the host can dictate`.
- `messages from another origin are ignored`.

- [ ] **Step 2: Run, verify failure** — `cd editors/productivity && npm test`; Expected: FAIL.

- [ ] **Step 3: Implement** `newEmailPage(options: { script: string; nonce: string }): string` and `newEmailView.ts`. Strings come from `EMAIL_COPY.newEmail`.

- [ ] **Step 4: Run** — `cd editors/productivity && npm test && npm run compile`; Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add editors/productivity/src/newEmailPage.ts editors/productivity/src/newEmailView.ts editors/productivity/esbuild.js editors/productivity/esbuild.test.js editors/productivity/test/new-email-view.test.ts
git commit -F <message file>   # "feat(productivity): the New email page: organization, layout cards, name and description (#6032)"
```

---

### Task 9: New email creates through the Materializer

Read first: front-door spec section 2 and amendment 5 (`startMaterialize`, `followComposition`, statuses, cancellation; a described Markdown hand-off does not apply to email); engine plan Task 12 (blank email: layout and no statement or sources; described email: statement, sources, `layoutId`); `editors/productivity/src/templateExamples.ts` (limits, MIME mapping, upload loop, being deleted); `editors/productivity/src/dictation.ts`; `editors/productivity/test/onePathCreate.test.ts` (front-door Task 7).

**Files:**
- Create: `editors/productivity/src/newEmail.ts`
- Modify: `editors/productivity/src/extension.ts` (register the `memql.productivity.newEmail` custom editor and command; remove `templateFromExamples` and `resumeEmailComposition`; test hook `newEmailReady`)
- Modify: `editors/productivity/package.json` (command `memql.productivity.newEmail` "New Email", category `MemQL`; `customEditors` entry `{viewType: "memql.productivity.newEmail", displayName: "New Email", selector: [{filenamePattern: "memql-file:/new-email/*/*"}], priority: "default"}`; remove the two old commands)
- Modify: `editors/productivity/src/dictation.ts` (`start(panel, lease, operation = "transcribe your feedback")`; `dictationProblem` takes the lease), `editors/productivity/src/markdownEditor.ts` (passes `(await this.load(document.uri)).lease`), `editors/productivity/src/documents.ts` (`transcribe(lease, …)`), `editors/productivity/test/dictation.test.ts`
- Modify: `editors/productivity/test/onePathCreate.test.ts` (allowlist: remove `templateExamples.ts`; add `newEmail.ts` "uploads reference files and pictures the person picked for a new email"; `emailEditor.ts` was added in Task 7; the planted-violation case stays)
- Modify: `editors/productivity/test/host.ts`
- Delete: `editors/productivity/src/templateExamples.ts`, `editors/productivity/test/templateExamples.test.ts`
- Test: `editors/productivity/test/new-email.test.ts`

**Interfaces:**
- Consumes: `startMaterialize`, `followComposition`, `MaterializeCancelled`, `MaterializeFailed`, `MaterializeRequest` (front door + Task 1); `buildEmailLayouts`, `buildEmailRender` (engine); `organizations` (Task 6); `previewHTML`, `skeletonHTML` (Task 4); `newEmailResource`, `resourceFrom` (Task 3).
- Produces:

```ts
export class NewEmail implements vscode.CustomReadonlyEditorProvider<vscode.CustomDocument> {
  constructor(context: vscode.ExtensionContext, api: EditorConnectionAPI, dictation: DocumentDictation);
  openCustomDocument(uri: vscode.Uri): vscode.CustomDocument;
  resolveCustomEditor(document: vscode.CustomDocument, panel: vscode.WebviewPanel): Promise<void>;
  open(): Promise<void>;                                      // the palette command
  whenShown(uri: vscode.Uri): Promise<{ state: string }>;     // test hook
}
```

Behaviour:
- **Command**: no lease gives `UserInputError(host.connectFirst)`; no active organization gives `host.addOrganization`; otherwise `vscode.open(newEmailResource(lease.domain, remembered ?? first.id))`.
- **Page**: the domain and preselected organization come from the link. When `api.current()?.domain` differs, `setup` says not connected; the Connect act calls `api.connect(domain)`; `api.onDidChange` re-runs setup. Setup loads `organizations` and `emailLayouts({accountId})`, then renders thumbnails with `emailRender({document, accountId})` (three at a time, cached for the session by row id and `revision`).
- **Request** (one `requestId = crypto.randomUUID()` per click; ignored while one is running):

```ts
{ name, format: "json", outputKind: "email_template", accountIds: [organizationId], layoutId, requestId,
  // Write it only:
  statement: description.trim(),
  sources: references.map(r => ({ kind: "library_file", ref: r.fileId, label: r.label, content: true, ...(r.include ? { includeImages: true } : {}) })) }
```

  The name is trimmed and loses a trailing `.email.json`. The full request is stored in `workspaceState["memql.newEmail.pending:" + link]` before `startMaterialize`; the composition id is added once known. `followComposition({execute, signal})` runs with an `AbortController` (Cancel aborts it, which cancels the composition) and an `execute` wrapper that throws a private `PageClosed` error once the panel is disposed (so closing stops polling without cancelling).
- **Result**: open `memql-file://<domain>/artifacts/<artifactId>/<name ending .email.json>` with `vscode.openWith(…, "memql.productivity.email", {viewColumn: panel.viewColumn, preview: false})`, dispose the panel, clear the pending and draft records, store the organization in `globalState`. `MaterializeCancelled` posts `idle`; `MaterializeFailed` and other errors post `failed` with `reportProblem(error, "write the email")`; both clear the pending record.
- **Resume**: on `ready`, a pending record with a composition id follows it; one without resends the same request (the server returns the same composition for the same `requestId`).
- **References**: `attach` shows a quick pick (`host.addReferences`: `host.fromComputer`, `host.fromFiles`). From this computer: `showOpenDialog` (`host.chooseReferences`, label `host.addLabel`, png, jpg, jpeg, gif, zip, txt, md, html, css, json, csv, svg), at most 64 files and 16 MiB in total checked before the first upload, each uploaded with `api.uploadFile(lease, name, mime, bytes)` (the MIME table from `templateExamples.ts`). From Files: a multi-pick (placeholder `host.recentFiles`) of `libraryFilesForOwner` rows with an allowed extension. Pictures (png, jpg, jpeg, gif) and ZIPs get the include switch. The draft (`workspaceState["memql.newEmail.draft:" + link]`) keeps organization, layout, name, description and references.
- **Dictation**: `dictation.start(panel, lease, "transcribe your description")`.

- [ ] **Step 1: Failing tests** (`test/new-email.test.ts`). `newEmailFixture(options)` builds a `NewEmail` page for `memql-file://client.example/new-email/client-org/New%20email` with a fake panel, a fake `context` (`workspaceState` and `globalState` over one `Map`, exposed as `state`) and a fake `api` whose `execute` scripts `clientAccountsAll`, `emailLayouts`, `emailRender`, `composeMaterialize` (`[{compositionId: "c1"}]`), `compositionById` (`ready` with `outputFileId: "f1"`), `libraryArtifactForFile` (`{id: "a1", title: "Autumn welcome.email.json"}`) and `composeCancel`, with the given references already uploaded. It exposes `receive(message)` (delivers a view message and resolves when its handling settles), `calls`, `opened` (the `vscode.openWith` calls), `disposed` and `link`.

```ts
test("Write it creates one email through the Materializer with the layout, organization, description and references", async () => {
  const f = newEmailFixture({ references: [{ fileId: "f-logo", label: "logo.png", include: true }, { fileId: "f-brief", label: "brief.md", include: false }] });
  await Promise.all([
    f.receive({ type: "write", layoutId: "builtin:article", name: "Autumn welcome.email.json", description: "A short welcome for new subscribers." }),
    f.receive({ type: "write", layoutId: "builtin:article", name: "Autumn welcome.email.json", description: "A short welcome for new subscribers." }),
  ]);
  const starts = f.calls.filter(c => c.name === "composeMaterialize");
  assert.equal(starts.length, 1, "a double click must start one composition");
  for (const pattern of [/layoutId: "builtin:article"/, /accountIds: \["client-org"\]/, /outputKind: "email_template"/, /format: "json"/, /name: "Autumn welcome"/, /statement: "A short welcome/, /requestId: "/])
    assert.match(starts[0].call, pattern);
  assert.match(starts[0].call, /\{[^{}]*ref: "f-logo"[^{}]*includeImages: true[^{}]*\}/);
  assert.doesNotMatch(starts[0].call.match(/\{[^{}]*ref: "f-brief"[^{}]*\}/)![0], /includeImages/);
  assert.deepEqual(f.opened, [["memql-file://client.example/artifacts/a1/Autumn%20welcome.email.json", "memql.productivity.email"]]);
  assert.equal(f.disposed, true);
  assert.equal(f.state.get(`memql.newEmail.pending:${f.link}`), undefined);
});
```

Also:
- `Start from the layout sends no description or references`.
- `each click has its own request id; a retry after a failure uses a new one`.
- `a reload while starting resends the same request id and follows the same composition`.
- `closing the page stops following without cancelling, and reopening resumes and opens the result once`.
- `Cancel stops the composition and keeps everything typed`.
- `a failed composition shows the problem and keeps the draft`.
- `the page connects only when asked`.
- `layout cards render through emailRender for the chosen organization, and an organization change reloads them`.
- `references upload once each and respect the limits` (65 files refused with `host.tooMany` and nothing uploaded; over 16 MiB refused; a `.exe` refused with `host.unsupported`).
- `the palette command opens the New email link for the remembered or first organization` (`vscode.open` with `memql-file://client.example/new-email/client-org/New%20email`; no lease and no organization give their sentences).

`test/dictation.test.ts`: the transcription receives the lease (`transcribe(lease, …)`), and `start` with an operation reports problems with it.

Host suite: after the email part, `vscode.open(vscode.Uri.parse("memql-file://cluster.example/new-email/client-org/New%20email"))`; the active tab's input is a `TabInputCustom` with `viewType === "memql.productivity.newEmail"`; `newEmailReady(uri)` gives state `Not connected to cluster.example`; the command `memql.productivity.newEmail` is registered and the old `newTemplate`, `templateFromExamples`, `resumeEmailComposition` are not.

- [ ] **Step 2: Run, verify failure** — `cd editors/productivity && npm test`; Expected: FAIL.

- [ ] **Step 3: Implement**; delete `templateExamples.ts` and its test; update the guard's allowlist and run its planted-violation case.

- [ ] **Step 4: Run everything this touched**

```bash
cd editors/productivity && npm test && npm run compile && cd ../..
node editors/productivity/test/runner.cjs && node editors/productivity/test/runner.cjs --desktop
MEMQL_VSCODE_VERSION=1.91.0 node editors/productivity/test/runner.cjs --desktop
node --test editors/browser/test/handoff.test.mjs
```

Expected: PASS. The 1.91.0 desktop run proves the selector `memql-file:/new-email/*/*` matches on the minimum engine (Rulings 2); if it does not, stop and report rather than widening the pattern to `**/new-email/*/*`, which would also capture local folders.

- [ ] **Step 5: Commit**

```bash
git rm editors/productivity/src/templateExamples.ts editors/productivity/test/templateExamples.test.ts
git add editors/productivity/src/newEmail.ts editors/productivity/src/extension.ts editors/productivity/package.json editors/productivity/src/dictation.ts editors/productivity/src/markdownEditor.ts editors/productivity/src/documents.ts editors/productivity/test/dictation.test.ts editors/productivity/test/onePathCreate.test.ts editors/productivity/test/host.ts editors/productivity/test/new-email.test.ts
git commit -F <message file>   # "feat(productivity): New email writes into a layout through the Materializer (#6032)"; body: starts consuming emailLayouts and composeMaterialize.layoutId; New Email replaces New Email Template, Create Email from Examples and Open Last Email Composition
```

---

### Task 10: Copy, commands and the productivity copy lint

Read first: `editors/vscode/test/copyLint.test.ts` (the three sources and rules); the UX record's "Audience and voice" and "Tree views and menus"; `editors/productivity/src/problemUI.ts`.

**Files:**
- Create: `editors/vscode/test/copyScan.ts` (the reusable parts moved out of `copyLint.test.ts`: `RULES`, `Copy`, `Allowed`, `manifestCopy(pkgRoot)`, `sourceCopy(srcDir, pkgRoot)`, `visibleText`, `pageLines(html, lineClass)`, `callArguments`, `literalText`, `literalFragments`, `propertyText`, `problems(copy, allowed, used)`)
- Modify: `editors/vscode/test/copyLint.test.ts` (imports them; its cases and results are unchanged)
- Create: `editors/productivity/test/copyLint.test.ts`
- Modify: `editors/productivity/package.json` (every command's category `MemQL`; `Markdown: Review` becomes `Markdown Review` with short title `Review`; `Markdown: Insert Image` becomes `Insert Image into Markdown`; description `Documents, emails, PDFs and human-reviewed suggestions connected to MemQL.`)
- Modify: `editors/productivity/src/problemUI.ts` (the error toast's button is `Show details`, which reveals the output channel holding the reference line; no modal), `editors/productivity/test/support/vscode.ts` (`showErrorMessage`)
- Test: `editors/productivity/test/problems.test.ts`

**Interfaces:**
- The productivity twin's rules: the core `RULES`, plus `Materializer`, `Nexus`, and `/\bCampaigns\b/`; every contributed command's category is `MemQL`; no title starts with `<Word>: `; every `EMAIL_COPY` string passes the rules, its state strings (`notConnected`, `notConnectedTo`, `chooseOrganization`, `chooseLayout`, `nameIt`, `ready`, `writing`, `failed`, `layoutsFailed`, `noOrganization`, `unreadable`, `local`, `issuesOne`, `issuesMany`, `unsaved`, `draft`, `published`, `changed`, `publishing`, `savingLayout`, `savingCopy`, `layoutSaved`) have at most six words with a placeholder counted as one, and every other string at most 25 words; the static text of `emailPage(…)` and `newEmailPage(…)` passes the rules; `modal: true` appears only where `ALLOWED_MODALS` says why (one entry: `extension.ts` Use Compared Revision, "the next save will overwrite the newer cluster copy").

- [ ] **Step 1: Failing tests** (`editors/productivity/test/copyLint.test.ts`): `package.json copy is plain and every command is in the MemQL category`; `page copy is plain and status lines are at most six words`; `toasts, input boxes and progress titles are plain` (scan `src`); `no user copy names Materializer, Nexus or Campaigns`; `only allowed modals remain`; `the scans read what they exist for` (positive controls, as the core test has); `every exemption still matches`. `test/problems.test.ts`: `Show details reveals the output instead of a modal`.

- [ ] **Step 2: Run, verify failure** — `cd editors/productivity && npm test`; Expected: FAIL (categories, the modal, the manifest titles).

- [ ] **Step 3: Implement** the move of the scan helpers, the twin, the manifest edits and the `problemUI` change.

- [ ] **Step 4: Run**

```bash
cd editors/vscode && npm test && cd ../productivity && npm test && npm run compile && cd ../.. && npm --prefix editors/browser test
```

Expected: PASS (the core lint's results are unchanged).

- [ ] **Step 5: Commit**

```bash
git add editors/vscode/test/copyScan.ts editors/vscode/test/copyLint.test.ts editors/productivity/test/copyLint.test.ts editors/productivity/package.json editors/productivity/src/problemUI.ts editors/productivity/test/support/vscode.ts editors/productivity/test/problems.test.ts
git commit -F <message file>   # "chore(productivity): user copy in the extension's voice, held by a copy lint (#6032)"
```

---

### Task 11: A gallery for the email pages, and the critique

Read first: `editors/vscode/gallery/harness.ts`, `build.mjs`, `index.ts`, `shoot.mjs`; engine plan Task 4 (`Layouts()`, the Acme built-ins).

**Files:**
- Create: `component/compose/emaildoc/gallery_fixtures_test.go` (`TestWriteEditorGalleryFixtures`; skips unless `MEMQL_EMAIL_GALLERY_DIR` is set; writes `<dir>/<name>.json` per built-in layout with `id, name, description, document, subject, text, issues, html`, the html rendered with `RenderOptions{Preview: true}`)
- Create: `editors/productivity/gallery/build.mjs` (runs the Go writer into `dist-gallery/fixtures`, failing with `ERROR: go is required to render the email fixtures` when Go is missing; bundles `src/emailView.ts` and `src/newEmailView.ts` into `dist-gallery/`; bundles `gallery/index.ts` with `vscode` aliased to `test/support/vscode.ts`; writes `<id>.<theme>.html` and `index.html`)
- Create: `editors/productivity/gallery/index.ts` (`buildGallery(fixturesDir)`, scenarios wrapped with the core harness's `wrapDocument` for both themes), `editors/productivity/gallery/scenarios/newEmail.ts`, `editors/productivity/gallery/scenarios/emailEditor.ts`
- Modify: `editors/vscode/gallery/shoot.mjs` (`--dir=<path>`, resolved against the working directory; `OUT`, `SHOTS` and the Chrome profile derive from it; the default stays the core's `dist-gallery`)
- Modify: `editors/vscode/gallery/harness.ts` (`VSCODE_THEME_VARS` gains, for both themes, every variable the email pages read that it lacks, with the values VS Code's Dark Modern and Light Modern provide: `dark_modern.json` / `light_modern.json` in VS Code's `extensions/theme-defaults/themes/`, or, for keys those files inherit, the value read from a real webview with Developer: Open Webview Developer Tools)
- Modify: `editors/productivity/package.json` (scripts `"gallery": "node gallery/build.mjs"`, `"gallery:shoot": "node ../vscode/gallery/shoot.mjs --dir=dist-gallery"`), `editors/productivity/.gitignore` (`dist-gallery/`), `editors/productivity/.vscodeignore` (`gallery/**`, `dist-gallery/**`)
- Test: `editors/productivity/test/gallery-vars.test.ts`

Scenarios (each in light and dark; photographed at 1000 and 520):

| id | What it shows |
|---|---|
| `new-email-choose` | one organization, four built-ins, nothing chosen |
| `new-email-ready` | two organizations, an organization layout plus the built-ins, Article chosen, a name, a description, two references (one picture switched on) |
| `new-email-writing` | the Article skeleton, Writing your email, Cancel |
| `new-email-failed` | Couldn’t write the email with the notice and Details |
| `new-email-disconnected` | Not connected to memql.localhost, Connect |
| `email-design` | Article rendered, `intro` selected, Draft, Save as layout and Publish |
| `email-design-mobile` | Mobile width, the button selected |
| `email-issues` | Confirmation with the hero picture empty: issue listed, picture acts, Fix 1 issue to publish |
| `email-published` | Published to Acme Co, Theme open |
| `email-custom` | a custom HTML email: rendered, Written in HTML, Preview with open |
| `email-loading` | the skeleton before the first render |
| `email-disconnected` | Not connected, the skeleton without sweep |

- [ ] **Step 1: Failing test** (`test/gallery-vars.test.ts`): every `--vscode-*` name in the static output of `emailPage(…)` and `newEmailPage(…)` and in the sources of `src/emailView.ts` and `src/newEmailView.ts` has a value in `VSCODE_THEME_VARS.light` and `.dark`.

- [ ] **Step 2: Run, verify failure** — `cd editors/productivity && npm test`; Expected: FAIL (missing variables).

- [ ] **Step 3: Implement** the harness variables, the Go writer, the build, the scenarios and `--dir`.

- [ ] **Step 4: Build, shoot, verify**

```bash
cd editors/productivity && npm test && npm run gallery && npm run gallery:shoot && cd ../..
(cd component/compose && GOWORK=off go vet ./... && GOWORK=off go test ./... -count=1)
go test ./component/compose/emaildoc/ -run TestWriteEditorGalleryFixtures -count=1 -v   # SKIP without the variable
make arch-model-check
cd editors/vscode && npm test && npm run gallery
```

Expected: 24 pages and 48 shots in `editors/productivity/dist-gallery/` (git-ignored); every test passes; the core gallery still builds.

- [ ] **Step 5: Critique** (frontend-design's last pass). Open `dist-gallery/index.html` and the shots side by side and check, fixing in the task that owns the code before committing:
  - no horizontal scroll at 520; the action bar never wraps its acts and its state never collides with them;
  - the selected block's outline is visible on light and dark emails, on the tinted section and on the brand-colour Confirmation canvas;
  - each skeleton reads as its layout's shape; card thumbnails crop at the same height and their names align;
  - muted text keeps its contrast in both themes; focus outlines are visible on every control;
  - the inspector reads as one quiet column: remove any element whose job another already does (one accessory off).

- [ ] **Step 6: Commit**

```bash
git add component/compose/emaildoc/gallery_fixtures_test.go editors/productivity/gallery editors/productivity/package.json editors/productivity/.gitignore editors/productivity/.vscodeignore editors/productivity/test/gallery-vars.test.ts editors/vscode/gallery/shoot.mjs editors/vscode/gallery/harness.ts
git commit -F <message file>   # "test(productivity): gallery of the email pages in both themes, wide and narrow (#6032)"
```

(Add any page or view file the critique changed to the same `git add`.)

---

### Task 12: Documentation

Read first: `editors/productivity/README.md` (email sections, about lines 185 to 232, and the offline bullet near the top); `docs/public/language/vscode.md` ("Create Email from Examples" paragraph); `docs/public/operate/campaign-sending.md` ("Templates in Productivity Tools" and the Use in Campaigns sentence above it). The engine and OS plans edit adjacent lines in `campaign-sending.md`: on merge keep both sides.

**Files:**
- Modify: `editors/productivity/README.md`, `docs/public/language/vscode.md`, `docs/public/operate/campaign-sending.md`

- [ ] **Step 1: Rewrite** in current-only language (no "previously"):
  - README: **New Email** (organization, layouts including the four built-ins, name, Start from the layout or describe it with references and pictures to use; the email is written into the layout's content blocks while brand blocks stay; closing the page does not stop the writing, and opening New Email for the organization picks it up); **the email editor** (Design at desktop or mobile width or as plain text, click a block, the outline's move, add and remove, pictures from Files or uploaded, Theme, Preview with; Source and Split; Design needs the cluster, Source works offline); **Publish** (saves, then makes the email a template in the Email app for its organization; publishing the same file again updates that template; refused while there are issues) and **Save as layout**; local email files use Save to MemQL first; limits (references up to 64 files and 16 MiB, pictures up to eight and 1 MiB in total, an email up to 2 MiB). Remove Create Email from Examples, Open Last Email Composition, Use in Campaigns and Sample values.
  - `vscode.md`: the same in one paragraph.
  - `campaign-sending.md`: Use in Campaigns becomes Publish; Create from examples becomes New Email.

- [ ] **Step 2: Run** — `go test ./docs/... -count=1`; Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add editors/productivity/README.md docs/public/language/vscode.md docs/public/operate/campaign-sending.md
git commit -F <message file>   # "docs: writing an email in VS Code (#6032)"
```

---

### Task 13: Install the build for the owner

The owner tests editor work by hand before merge. Tell the owner before installing: the window reloads, and the build replaces the installed Productivity Tools.

**Files:**
- Modify: `editors/productivity/package.json` and `editors/productivity/package-lock.json` (`version` 0.2.0, so the installed build is recognisable)

- [ ] **Step 1: Build and install**

```bash
npm --prefix editors/productivity version 0.2.0 --no-git-tag-version
bash scripts/vscode/deps.sh
npm ci --no-audit --no-fund --prefix editors/vscode && npm --prefix editors/vscode run compile
npm ci --no-audit --no-fund --prefix editors/productivity && npm --prefix editors/productivity run package
ls editors/productivity/memql-productivity-tools-0.2.0.vsix
code --install-extension editors/productivity/memql-productivity-tools-0.2.0.vsix --force
```

Expected: the VSIX exists and `code` prints that it was installed. The owner runs Developer: Reload Window.

- [ ] **Step 2: Tell the owner what the build needs.** New Email and the Design preview call `emailLayouts`, `emailRender`, `campaignSaveLayout`, `templateForSource` and `composeMaterialize.layoutId`; until the cluster runs this branch's engine they show Couldn’t load the layouts or Couldn’t show this email. The local deploy (`make dev`) happens only after the owner says go. The browser editor at `https://vscode.<domain>/editor/` changes only with the edge image (`make dev NODE=edge`); the controller schedules that live step with the owner.

- [ ] **Step 3: Commit**

```bash
git add editors/productivity/package.json editors/productivity/package-lock.json
git commit -F <message file>   # "chore(productivity): 0.2.0 for the owner's hand test (#6032)"
```

---

### Task 14: Close-out (after the owner's hand test)

- [ ] **Step 1:** Fold the owner's findings into the task that owns each piece, rerun that task's Step 4, and reinstall (Task 13 Step 1).
- [ ] **Step 2:** In the epic's own merge, delete this plan (`git rm docs/superpowers/plans/2026-10-11-vscode-email-editor.md`; `docs/CLAUDE.md`: a plan is spent when it ships) and commit `docs: the VS Code email editor plan is spent (#6032)`.
- [ ] **Step 3:** Remove `node_modules` from this scratch worktree when no other task still needs them (`editors/vscode`, `editors/productivity`, `editors/browser`, `sdk/ts`, `sdk/ts-viewkit`).
