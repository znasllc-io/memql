# Email composition: wireframe layouts, brand kits and a Claude Code harness step

- **Date:** 2026-11-04
- **Status:** design approved by the owner section by section on 2026-11-04 (brainstorm held in the memql-fylo session); this written spec awaits the owner's review
- **Program:** the Fylo email program, sub-project **A** of four:
  - **A** (this spec): compose an email from a chosen layout, the account's brand kit, a description and attached material, through the harness on the Apps route.
  - **D**: email as code. Templates and brand kits declared in a product repository, seeded into the account as drafts on deploy, exported from the Email app. Designed separately; independent of A, built in parallel.
  - **B**: Fylo's eleven emails (Welcome 1 and 2, Stage B 1 to 9) composed with A, carried to production with D, and wired into their audience, sender and journeys.
  - **C**: creating a template from the OS without waiting, and autosave into the template.
- **Builds on:** `2026-10-11-email-layouts-automations-design.md` (the email document, the four built-in layouts, journeys) and `2026-10-11-materializer-front-door-design.md` (whose ruling for emails this spec changes; see section 9).
- **Deadline context:** Fylo's engagement closes 2026-11-20. A and D should be live on the production installation by about 2026-11-13, leaving B a week.

## In plain terms

- A person picks a layout from a grid of wireframes, describes the email, and attaches material: a ZIP or pictures, and optionally a picture of how the finished email should look. memQL's harness composes the email in Claude Code, on the Apps route, and leaves a **draft** template in the chosen account.
- **Layouts are structure only.** They are drawn in the grid as wireframes: grey panels, no colour, no words, no pictures. Each account's **brand kit** supplies the look: colours, fonts, logo, signature, follow line, social links, footer and a standing reply.
- **24 built-in layouts**: the four from 2026-10-11, three more that Fylo's series needs, and seventeen common email patterns. Nobody creates layouts for now.
- **Nothing waits.** "Write it" creates the template at once and opens it; the harness fills it in while the editor says so.
- **From Ask**, the person attaches the material and describes the email; the harness suggests the closest layout, shows its wireframe, and composes once the person confirms.

## Decisions

Taken with the owner on 2026-11-04.

| Question | Decision | Why |
|---|---|---|
| What an attached ZIP is | Raw material for composing an email (pictures, copy, optionally a picture of the finished email). Not a finished email to copy byte for byte | The owner's correction of an earlier reading |
| Where a composed email's look comes from | A **brand kit per account** | Layouts become colourless frames, so every layout is usable by every account |
| How a layout is chosen in Ask | The harness suggests the closest layout with its wireframe; the person confirms or names another before anything is written | A conversation has no grid |
| How composition runs | A tool-using **harness step that runs whole in Claude Code** on the Apps route, checks its own render, and submits | The only option in which the model sees the pictures without leaving the Apps route (a separate vision call is never re-routed by a route choice, `docs/public/operate/ai-routing.md`); it checks its own work; it reuses app sessions and memQL's MCP tools |
| Model route | **Apps only**, "for now". No fallback to local models | Owner direction |
| Publishing | A run never publishes; it leaves drafts | Publishing stays an explicit human action |
| Creating layouts | Not offered for now. "Save as layout" and the OS Layouts list are hidden | Owner direction; the grid holds the built-in layouts |
| How the picker shows layouts | A grid of wireframes, grouped by category | Owner direction |

## 1. Layouts

### 1.1 What a layout is

A built-in layout stays a layout-mode email document (`component/compose/emaildoc/layouts/<name>.email.json`, id `builtin:<name>`). What changes:

- **Colour roles only.** Blocks name colour tokens (`page`, `canvas`, `primary`, `secondary`, `accent`, `tint`, `text`, `muted`, `white`), as Parse already requires. A layout's own theme is the neutral grey example theme, used only to preview an empty layout. **Confirmation** is rebuilt so its coloured panel is a section with a background token rather than a canvas colour of its own, so any brand kit fits it.
- **No brand.** A layout carries no account's words, pictures or logos (already the rule for built-ins).

### 1.2 Brand slots

The blocks that repeat in every email of an account have reserved top-level ids that every layout shares. The brand kit fills them; the layout decides where they sit and how large they are.

| Slot | Block | Filled from the kit with |
|---|---|---|
| `logo` | image | picture, alt text, optional link |
| `signature` | columns or section holding a photo, a name and a title | photo picture; name as a picture or as text; title line |
| `follow` | heading | one line of text |
| `social` | social | each network's link and icon picture |
| `footer` | footer | lines and links (preferences, unsubscribe and view-in-browser always the engine's merge tags) |
| `reply` | section holding a heading, a button and a note | heading text, button label and link, note text |

- A slot block has role `brand`; composition never writes into it (`emaildoc.Merge` already refuses brand blocks).
- **A slot the kit leaves empty is removed** from the email, with the spacing the layout gives it.
- The existing layouts are brought to these ids. Article's `question`, `question-more` and `ask` blocks become one `reply` section.

### 1.3 Flexible blocks

Kept in the layout catalogue (Go, beside each entry), not in the document format, so the canonical `*.email.json` bytes do not change:

- **Optional** content block ids: composition may drop them when there is nothing for them.
- **Repeatable** content blocks with a range (for example `step` 2 to 6, `story` 1 to 3): composition may repeat the block, with ids suffixed `-2`, `-3`, and so on, within the range. (revised: see Planning revisions)

Any other change to a layout's structure is refused at submit (section 3.3).

### 1.4 Wireframes and the grid

- `emaildoc.Wireframe(doc) string` draws a layout as a deterministic SVG (600 units wide, as tall as its blocks), in neutral greys only:
  - image: a box with a picture mark, at the block's width and height;
  - heading: one or two bars, as thick as its size;
  - text: three lines, honouring alignment; highlight: a light box holding two lines;
  - button: a pill outline; divider: a line; spacer: a gap;
  - section: an outlined panel around its children; columns: children side by side;
  - social: a row of circles; footer: a block of short centred lines.
- The `emailLayouts` query returns each layout's `wireframe` and `category` with its name and description.
- VS Code's New email page shows the grid grouped by category and **renders no previews** (today it renders every layout's full preview first, three at a time, which is part of its "Loading layouts" wait). Ask's layout suggestion shows the same wireframe.

### 1.5 The catalogue

Every layout stays within the engine's limit of 8 distinct pictures **with a full brand kit** (logo, signature photo and name, three social icons) and the most content pictures its repeatable blocks allow. A test enforces it.

| Id | Category | Structure | Fylo |
|---|---|---|---|
| `confirmation` | Welcome | logo, big headline and subhead, hero picture, confirm button (`{{confirmUrl}}`), note, footer | Welcome 1 |
| `welcome-tour` | Welcome | logo, hero picture, headline, three benefits (icon and line), button, social, footer | |
| `letter` | Letters | logo, headline, letter, button, signature band, promo (text beside a picture), follow, social, footer | Welcome 2 |
| `plain-note` | Letters | logo, letter text, signature, footer; no content pictures | |
| `greeting` | Letters | logo, full-width picture, short message, signature, footer | |
| `article` | Articles | logo, banner picture, tinted body (intro, optional highlighted statement, article text), reply, signature, footer | Stage B 1, 4, 6, 9 |
| `lesson` | Articles | logo, banner picture, tinted body (text, optional highlighted statement), promo (text beside a picture), follow, social, footer | Stage B 2, 5 |
| `three-reasons` | Articles | logo, banner picture, row of three icons, tinted body (lead-in heading, three titled sections, closing statement), reply, signature, footer | Stage B 8 |
| `side-by-side` | Products | logo, banner picture, tinted intro and statement, two cards (icon, title, subtitle, text), closing lines, reply, signature, footer | Stage B 7 |
| `spotlight` | Announcements | logo, large picture, short message, button, social, footer | Stage B 3 |
| `announcement` | Announcements | logo, hero picture, headline, short text, button, social, footer | |
| `video` | Announcements | logo, headline, video picture, text, watch button, social, footer | |
| `product-launch` | Products | logo, product picture, headline, feature list, button, social, footer | |
| `product-grid` | Products | logo, headline, 2 by 2 products (picture, name, line, button), social, footer | |
| `product-focus` | Products | logo, picture beside name, text and button, customer quote, footer | |
| `offer` | Promotions | logo, large offer headline, code panel, button, fine print, social, footer | |
| `newsletter` | News | logo, intro, 1 to 3 stories (picture, title, excerpt, link), social, footer | |
| `steps` | Guides | logo, headline, intro, 2 to 6 numbered steps, button, reply, footer | |
| `picture-steps` | Guides | logo, headline, 2 to 4 rows of a picture beside text, footer | |
| `questions` | Guides | logo, headline, 2 to 6 question and answer pairs, reply, footer | |
| `event` | Events | logo, picture, headline, details panel (date, time, place), button, footer | |
| `testimonial` | Feedback | logo, large quote, photo and name, button, social, footer | |
| `review-request` | Feedback | logo, headline, product picture, short text, button, footer | |
| `thank-you` | Feedback | logo, headline, text, tips panel, reply, signature, footer | |

`confirmation`, `letter`, `article` and `spotlight` exist and keep their ids; `lesson`, `three-reasons` and `side-by-side` are new and needed by B; the other seventeen are new. Every layout meets the existing contrast test (WCAG 2 AA for every text and background pair) with the neutral theme.

## 2. The brand kit

### 2.1 Contents

One kit per account:

- **theme**: the eight colours (`#rrggbb`), `headingFont` and `bodyFont` (each rendered with an email-safe fallback stack), and `webFonts`, as the document's `Theme` already has them.
- **logo**: `{src, alt, href?}`. (revised: see Planning revisions)
- **signature**: `{photo?, name: {src} | {text}, title?}`. (revised: see Planning revisions)
- **follow**: one line of text.
- **social**: a list of `{network, href, icon}`.
- **footer**: `{lines[], links[{label, href}], separator?}`. (revised: see Planning revisions) It **must** carry a `{{unsubscribeUrl}}` link, so no email built from the kit needs the engine's appended footer; preferences and view-in-browser use `{{preferencesUrl}}` and `{{viewUrl}}`.
- **reply**: `{heading, button: {label, href}, note?}`. A link is a web address, `mailto:` or `tel:`, as emaildoc already accepts.

Every part except theme, logo and footer is optional; an empty part empties its slot (1.2).

### 2.2 Storage and API

- **Concept** `v1:campaigns:brandKit`: `ownerUserId` (attribution), `accountId` (fixed at creation; one kit per account), `kit` (the object above). `@rowAuthz(owner="ownerUserId", clusterOwner, account="accountId")`, as templates have. The revision is the row version, as for templates.
- **Pictures are stored inside the kit** as `data:` PNG, JPEG or GIF. Library pictures can be read only by whoever uploaded them (`component/campaigns/store.go` `LibraryFileForArtifact`), so a kit holding `artifact:` references would break for every colleague. Saving accepts `artifact:<id>` (read under the saver, who uploaded it) or `data:`, and stores `data:`. (revised: see Planning revisions)
- **Builtins:** `campaignSaveBrandKit(accountId!, kit!, expectedRevision)` returns `{revision, warnings}`. It needs the same organization write permission as `campaignSaveTemplate`. It refuses an invalid kit with a sentence naming the field. **Contrast never refuses:** each text and background pair the layouts draw that falls under WCAG 2 AA comes back as a warning. Fylo's white headline on teal is about 2.3 to 1, and refusing it would make Welcome 1 impossible to reproduce. It also warns when the kit's pictures exceed 512 KiB, half an email's picture budget.
- **Query:** `brandKitForAccount(accountId)`.

### 2.3 Applying a kit

`emaildoc.ApplyBrand(layout Document, kit BrandKit) (Document, []Issue)`:

- replaces the layout's theme with the kit's;
- fills each slot block from the kit, keeping the layout's sizes, padding and alignment;
- removes slots the kit leaves empty.

It is used when a template is created from a layout (both buttons, section 3), by the composition context tool, and by the Brand page's preview. A template keeps the brand-applied document it was created with: changing a kit later does not rewrite existing templates.

### 2.4 The Brand page

OS → Email → Settings → **Brand**, per organization: (revised: see Planning revisions)

- colour swatches, a font list, picture uploads (logo, signature photo and name, social icons), footer lines and links, the follow line and the reply;
- a live preview of a sample layout through `emailRender`;
- the contrast and size warnings listed beside the fields;
- Save with the revision check, like templates.

### 2.5 As code

D carries the kit like templates: `email/brand.json` with its picture files in a product repository, seeded on deploy, exported from the Brand page. D's spec settles the rules.

## 3. Composition

### 3.1 Starting it

**VS Code, New email** (`editors/productivity/src/newEmail*.ts`):

- organization; a layout from the grid; a name; a description;
- files from the computer or from Files: pictures, ZIPs and text files, within today's 64 files and 16 MiB;
- an optional **picture of the finished email**;
- two buttons: **Start from the layout** and **Write it**.

**Ask:** the person attaches files and asks for an email. Ask's agent finds the email composition capability and calls `campaignComposeEmail` without a layout (3.2). (revised: see Planning revisions) That starts a composition goal on the Apps route, **whatever route the conversation itself uses**. The goal:

1. works out the account, asking if it is not clear;
2. suggests the closest layout from the catalogue, comparing the attached picture of the finished email (when there is one) and the material with the layouts' structures, and asks the person (`requestUserFeedback`) with the layout's name, its wireframe and two alternatives;
3. creates the template once the person confirms or names another, and composes it (3.3). (revised: see Planning revisions)

### 3.2 Nothing waits

Two new builtins:

- `campaignCreateTemplateFromLayout(accountId!, name!, layoutId!)`: applies the account's kit (or the neutral look when the account has none) and saves a draft. Returns `{templateId, revision}` at once. **Start from the layout** calls it and opens the template.
- `campaignComposeEmail(accountId!, description!, fileIds!, referenceFileId?, layoutId?, name?)`: refuses when the account has no brand kit, or when the caller has no Apps machine connected (3.5). With a `layoutId` (**Write it**), it creates the template as above, marks it writing, starts the goal, and returns `{templateId, goalId}` at once. Without one (Ask), it starts the goal, which creates the template after the person confirms the layout. (revised: see Planning revisions)

The template row gains an optional **`composition`** object: `{goalId, state: writing | done | failed, startedAt, finishedAt?, error?}`.

- The editor opens the template at once. While `state` is `writing` it shows "MemQL is writing this email", the elapsed time and **Cancel**, and is read-only; when the run submits, the composed email replaces the content in place.
- The OS Templates list shows the template at once, marked **Writing**, or **Composition failed** with the reason and **Retry** (which starts a new goal with the recorded inputs). (revised: see Planning revisions)
- Emails no longer go through the Materializer: no Library file and no `composeMaterialize` run is involved. The editor already opens and saves a template that has no Library file behind it (`templates/<id>`). Existing templates linked to Library files keep working as they do.

### 3.3 The run

- **The goal** is created as the caller, with source `route:apps`, level `strong`, and a new `requestedVia` value `email`, recorded with its inputs (account, template, layout, description, files, reference picture).
- **One tool-using step**, `composeEmail`, runs whole in Claude Code on the caller's own machine, through the existing session door. (revised: see Planning revisions) The step's prompt is a new DSL prompt `emailCompose`, `@level("strong")`.
- **Inputs land in the session workspace.** The step's files become the session handover's inputs, which Cockpit already pulls from the Library into the workspace. Today Ask's handover sets no inputs, and wiring them is part of A. A ZIP is checked on the server first by the hardened reader in `integrations/compose/source_content.go` (no paths leaving the archive, no nested archives, no links). Its limits are raised for composition to 256 entries and 64 MiB unpacked. Cockpit then unpacks it in the workspace by the same rules. (revised: see Planning revisions)
- **Picture handles.** The server gives every picture among the inputs and inside ZIPs a handle, `memql-asset:<sha256>` of its bytes, the form the composer already uses.
- **Its tools over MCP** (beside the session's own `submit` and `next_task`):
  - `emailComposeContext`: the layout (blocks with ids, types, roles and sizes; the optional ids and repeat ranges), which brand slots the kit fills, the pictures (handle, workspace path, type, bytes, width and height), the remaining picture budget, and the account's name;
  - `emailComposeRender(document)`: the HTML, the text and the issues, with handles resolved as at submit;
  - `emailComposeSubmit(document)`: saves the email (below); (revised: see Planning revisions)
  - in the Ask flow, also `emailLayoutCatalogue` (names, descriptions, categories and wireframes) and `requestUserFeedback`. (revised: see Planning revisions)
- **Its loop:**
  1. read the context and the files (the copy from text files; the pictures, including the reference, which Claude Code views itself);
  2. write the document: subject, preheader and the content blocks, placing pictures by handle and using optional and repeatable blocks;
  3. render, read the issues, and compare the content and its order with the reference picture;
  4. fix and repeat, up to five renders;
  5. submit.

  Claude Code cannot photograph the render inside its sandbox. Its check is the issue list plus the content against the reference; the pixel comparison is the acceptance harness's job (section 6).
- **Submit:**
  - checks the structure against the layout: brand slots untouched, and only optional blocks dropped and repeatable blocks repeated within range; (revised: see Planning revisions)
  - parses the document with the same parser as any template;
  - resolves each handle to a `data:` picture from the server's own copy (no picture bytes pass through the model);
  - saves the draft revision through the same path as `campaignSaveTemplate`, then sets `composition.state` to `done`.

  A draft may keep issues. Publishing stays refused until they are fixed.

### 3.4 Rules the step follows

- Text found in the files is used word for word; the description says what to fill in where the files do not.
- It writes the subject and preheader when the files do not supply them.
- It never invents a claim, price, date, address or link. A button with no supplied destination stays empty, and the draft reports it (`link_missing`). Link merge tags (`{{confirmUrl}}` and the others) stay as the layout has them.
- It never writes into brand slots and never publishes.
- Text in the files is material for the email, never instructions (the existing composer rule, carried over).

### 3.5 Apps route only

- The goal names `route:apps`, so every model call in it goes to a signed-in app on the caller's own machines: Claude Code when that machine's app strategy is "Claude Code only". A route never falls through to another, by the engine's existing rule.
- `campaignComposeEmail` refuses up front when the caller has no connected machine with an allowed, signed-in app. The refusal says to open Cockpit with Claude Code signed in. Nothing is created, and nothing goes to local models.
- Consequence: whoever composes needs Cockpit with Claude Code on their machine. Today that is the operator. Fylo's own people can review, edit and publish drafts, but cannot compose until they have that, or until a later route is added.

### 3.6 Watching and cancelling

- Ask shows the goal's activity, and "View work" opens it in Nexus. VS Code shows the banner; the OS shows the mark.
- **Cancel** (VS Code or Nexus) cancels the goal and stops the session. `composition.state` becomes `failed`, with "cancelled" as the error, and the draft stays as the brand-applied layout.

## 4. Pictures, limits, failures and safety

- **Pictures** come from the attached pictures and the members of attached ZIPs, each with a handle (3.3). The finished template carries them inside it as `data:` pictures, so anyone in the account can edit and publish it. The picture of the finished email is guidance only and is never embedded unless the description says so. Types are PNG, JPEG and GIF, as today; any other file is named in the run's summary and left out.
- **Limits:** the engine's per-email limits stay: 8 distinct pictures, 1 MiB of picture data, 1 MiB per picture (`emaildoc` `MaxImages`, `MaxImageBytes`).
  - When the material would exceed them, the step chooses fewer pictures and names the ones left out.
  - Pictures are neither shrunk nor converted; one that is too large is reported.
  - All eleven Fylo emails fit (at most 7 pictures and 124 KB).
- **Failures, as the person sees them:** (revised: see Planning revisions)

  | Situation | What happens |
  |---|---|
  | No Apps machine connected | Refused before anything is created, with the sentence in 3.5 |
  | The account has no brand kit | Write it is refused, with a link to the Brand page. Start from the layout still works, with the neutral look |
  | Unusable files (wrong type, damaged, too large) | Named in the summary; the run continues without them, and stops only when nothing usable is left |
  | Issues it cannot fix after five renders | It submits anyway; the draft lists the issues; publishing stays refused until they are fixed |
  | Cancel, or the machine sleeps or quits mid-run | The session stops; the draft stays as the brand-applied layout, with the reason and Retry |
  | The template changed during the run | Submit does not overwrite it: it saves its version as a new template beside it (`<name> (MemQL's version)`) and says so |

- **Safety:** files are untrusted. The session runs in the Cockpit sandbox with no network access. Submit parses the document with the same parser as any template: allowlisted markup, links that are web, `mailto:` or `tel:` addresses or the four link merge tags, and pictures checked by their content.

## 5. Engine changes by area

| Area | Change |
|---|---|
| `component/compose/emaildoc` | 20 new layouts; Confirmation rebuilt on tokens; Article's `reply` section; catalogue metadata (category, optional ids, repeat ranges); `Wireframe`; `BrandKit` type, its parse and `ApplyBrand`; the structure check used at submit; tests |
| `dsl/campaigns` (revised: see Planning revisions) | `brandKit` concept; the template's `composition` field; builtins `campaignSaveBrandKit`, `campaignCreateTemplateFromLayout`, `campaignComposeEmail`; query `brandKitForAccount`; `emailLayouts` shape gains `wireframe` and `category` |
| `component/campaigns` | Handlers for the builtins; the composition state; submit (structure check, handle resolution, save, copy on conflict); contrast and size warnings |
| `integrations/compose` | The hardened ZIP reader reused with composition's limits; the Materializer's `email_template` output and `emailContentInstructions` retired |
| `integrations/agent`, `integrations/work`, `app` | The `composeEmail` step and its MCP tools; the `emailCompose` prompt; goals with `route:apps` and `requestedVia: email` |
| `component/memql`, `cockpit/internal/worker/appsession` | Handover inputs from the step's files; ZIP unpacking in the workspace by the hardened rules |
| `editors/productivity` | New email page (wireframe grid, files, reference picture, the two buttons); the editor's writing banner, read-only state, live refresh and Cancel; "Save as layout" hidden; the one-path-create tests updated |
| `clients/os` | Email → Settings → Brand; Writing and failed marks with Retry in Templates; the Layouts list hidden; Ask showing the layout suggestion's wireframes |
| Docs | Public docs for composing emails and brand kits; `campaign-sending.md` and `memql-os.md` updated; the 2026-10-11 specs marked as superseded where section 9 says |

## 6. Testing and acceptance

**Engine:**

- **Layouts.**
  - Every layout parses and names itself.
  - Every layout meets the contrast test with the neutral theme.
  - Every layout stays within 8 pictures with a full kit and its repeatable maximums.
  - Every layout's wireframe matches a golden SVG.
- **Brand kit.**
  - Save checks the revision, stores pictures as `data:`, and returns contrast and size warnings without refusing.
  - The footer must carry `{{unsubscribeUrl}}`.
  - `ApplyBrand` replaces the theme, fills slots and removes empty ones.
  - Composition cannot write into a slot.
- **Flexible blocks.** Submit accepts drops of optional blocks and repeats within range, and refuses any other structural change.
- **Templates.**
  - Create from layout returns at once.
  - Compose marks the template writing.
  - Submit embeds pictures from their handles and saves a copy on conflict.
  - No run publishes.
- **The run.**
  - The tools work end to end against a stand-in Claude Code, as Cockpit's existing tests stand one in.
  - With no app connected, the refusal is immediate and no call reaches a local model.
  - ZIP rules hold at 256 entries and 64 MiB.

**Clients:**

- **VS Code.** The grid shows wireframes and calls `emailRender` for none of them. Both buttons open a template at once. The banner, the read-only state, the refresh in place and Cancel all work.
- **OS.** The Brand page; the Writing and failed marks and Retry; the Layouts list and "Save as layout" hidden.
- **Ask.** Attachments reach the session. The flow goes suggestion, confirmation, then composition.

**Acceptance on real material**, with B's material: (revised: see Planning revisions)

1. On a local stack, with Fylo's brand kit, compose each of the eleven emails from its own folder: its pictures, its plain-text copy, and a screenshot of the original as the reference picture, using the layout the table in 1.5 gives it.
2. Compare each result with the original with the existing fidelity harness (`scripts/email/fidelity.sh`: headless Chrome, 600 pixels wide, pixel difference). The target is its default limit, 2%.
3. A side-by-side sheet of all eleven goes to the owner for sign-off.

The material stays outside the repository, as that harness requires.

**Done means:**

- The grid shows all 24 wireframes with no wait.
- Both New email buttons open a template at once.
- The eleven Fylo emails compose within the acceptance limit, with every picture present.
- The same works from Ask with a layout suggestion and confirmation.
- No call goes to a local model.
- No run publishes.

## 7. Delivery

- The work ships in an engine release; the production installation is upgraded to it through its instance repository, as its other engine bumps are.
- Target: in production by about 2026-11-13, beside D, so B has a week before 2026-11-20.

## 8. Not in A

- creating or editing layouts (and organization layouts, which are hidden);
- resizing or converting pictures;
- composing without an Apps machine, or on any route but Apps;
- deploying templates and kits from code (D);
- the audience, sender, journeys and sign-up form (B);
- the OS wizard's wait and autosave into templates (C);
- refreshing existing templates when a kit changes.

## 9. Risks and points settled during implementation

- **The step cannot see its render.** It checks the issue list and the content against the reference; layout quality, the kit and the model carry the look. The acceptance harness is where a gap shows, and a gap there is fixed in the layout before B.
- **Two signature photos.** Welcome 2's band shows a rectangular photo, while the Stage B signatures show a round one. Either the kit carries both crops, or Letter's band takes its photo as content. Settle it during implementation with the acceptance harness.
- **Spotlight and the follow line.** Stage B 3 shows social icons with no follow line above them, so Spotlight uses the `social` slot without `follow`. Check it against the original in acceptance.
- **Session time.** A composition is one session. Check the session ceilings against real runs on the Fylo material, and record the times in the run's activity.
- **The front-door ruling changes for emails.** The 2026-10-11 front door sends every new file through the Materializer. Emails stop being Library files at creation: a template row is created directly. The ruling holds for every other file. `onePathCreate` tests change for emails only, and the 2026-10-11 specs get a note pointing here.
- **Hidden, not removed.** Organization layouts stay in the database, and templates made from them keep working.
- **Contrast warnings allow brand colours under AA.** The account's look wins, and the warning stays visible on the Brand page.
- **The deadline.** If A risks 2026-11-13, the seventeen common layouts beyond B's seven ship after the deadline, and the grid starts with the seven.

## Planning revisions

Written 2026-11-05, when the build was done. These are the ways the build differs from the spec as first written. Where the spec body now reads wrong, it carries the mark "(revised: see Planning revisions)".

1. **Ask's layout choice runs in Ask's own turn.** A question asked inside a Claude Code session ends that session, and only the agent reply loop resumes after an answer. So Ask's agent lists the layouts, asks with `requestUserFeedback` (`presentation: "layouts"`), then calls `campaignComposeEmail`. That builtin therefore always takes a `layoutId`. Ask's suggestion reads the description, the file names and the catalogue. The pictures are seen by the composition run.
2. **ZIPs are unpacked on the server.** Members become Library files in a folder named `Email material`, archived after the run. The session pulls them as ordinary inputs. Cockpit is unchanged, so composing needs no Cockpit update on anyone's machine.
3. **Slot blocks are found by id anywhere in the document.** Confirmation's logo sits inside its coloured panel. The footer stays at the top level, as the renderer requires.
4. **The step submits a composition, never a whole document.** A composition is the subject, the preheader, the content items, the drops and the repeats. The server rebuilds the email from the brand-applied layout, so brand pictures never pass through the model and the structure check is built in.
5. **Builtins beyond the spec's list.** These were added: `campaignComposeContext`, `campaignComposeRender` and `campaignComposeSubmit` (the step's tool handlers), `campaignRunComposition` (the run step), `campaignCancelComposition`, `campaignRetryComposition`, `campaignBrandPreview` and `campaignLayoutCatalogue`.
6. **The New email page has no per-file "Use in the email" switch.** The composer chooses among the attached pictures. The separate picture of the finished email is guidance only.
7. **The step runs as a per-user system agent, `emailComposerAgent`** (role `specialist`), because a session's tools need an acting agent.
8. **The real-material acceptance moved to sub-project B.** That is composing the eleven Fylo emails and comparing them with the fidelity harness, where Fylo's brand kit is created. A merges on the automated tests and the UI review.
9. **Kit pictures.** A picture in a kit is a `data:` PNG, JPEG or GIF, or `artifact:<id>`. An artifact is read as the caller and stored as `data:`. A size guard applies to both.
10. **The signature.** It may be one whole picture (`signature.picture`) as well as its parts, or instead of them.
11. **The logo.** It has two optional versions: `logo.onColour` for coloured panels and `logo.wide` for full-width headers. `ApplyBrand` picks between them, and each falls back to the logo.
12. **The footer.** It takes `lineGap`, the space between its lines.
13. **Repeatable blocks.** They start at their minimum (`ExpandToMinimum`) in compositions, in new templates and in wireframes.
14. **The catalogue.** `campaignLayoutCatalogue` rows carry no wireframes. Clients read wireframes from `emailLayouts`, and an Ask question gets them from the server. So the Ask flow's `emailLayoutCatalogue` tool gives names, descriptions, categories and outlines, not drawings.
15. **Presentation.** The `presentation` argument of `requestUserFeedback` is `enum("layouts")`. Leaving it out means a plain choice.
16. **The email tools.** Owned-work turns offer `emailLayoutCatalogue` and `startEmailComposition` only when the acting role is assistant.
17. **App reachability.** It is read through `(*MemQLEngine).AppReachableFor`, which answers on every node.
18. **Brand in the OS.** It is a page opened from a Brand row in Settings, with Save in its ActionBar. It is not an inline section.
19. **The OS Templates page.** It follows a composition through the live Templates feed, read again every 2 seconds while any template is writing.
20. **Start failures.** A composition whose goal could not start still opens its template, and the editor offers Retry.
21. **The Materializer refuses emails.** A request for one gets "Emails are made in the Email app." The old email output, its layout code and the clients' email choices were removed, along with the OS Layouts hooks and helpers that nothing used any more. The OS keeps reading layouts, only to name the layout a template was made from.
