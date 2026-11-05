# Email composition Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A person picks a layout from a grid of wireframes, describes an email and attaches material; a Claude Code harness step on the Apps route composes it with the account's brand kit and leaves a draft template, with nothing waiting.

**Architecture:** `component/compose/emaildoc` gains slot-aware layouts, brand kits, composition operations and wireframes. `component/campaigns` stores brand kits and a template's `composition` state, and serves the step's tools. A `@template` automation runs one tool-using AI call as a per-user system agent, so the Apps route hands it whole to Claude Code with exactly three tools. VS Code and the OS get the grid, the writing state, the Brand panel and Ask's layout choice.

**Tech Stack:** Go 1.26 (engine, campaigns, work, compose), memQL DSL edition 2026, TypeScript (VS Code productivity extension: node:test + esbuild; OS: React + vitest).

**Spec:** `docs/superpowers/specs/2026-11-04-email-composition-design.md` (same branch). Read it before any task.

## Global Constraints

- Branch: all work lands on `docs/email-composition-design` in the worktree `~/projects/memql/.claude/worktrees/email-composition-design`; one commit per task; commit messages read `area: sentence` like the repository's history and end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. No pull request: Task 27 merges into `main` after every check passes (owner instruction).
- `component/compose/emaildoc` imports only what `component/compose`'s go.mod already requires plus the parent package; the parent never imports emaildoc (doc.go:31-34). Check with `scripts/ci/module-boundaries.sh component/compose`.
- Pictures: PNG, JPEG, GIF only; per email at most 8 distinct pictures, 1 MiB of picture data, 1 MiB per picture (`MaxImages`, `MaxImageBytes`).
- Composition material: at most 64 files and 16 MiB as attached; a ZIP is read with 256 entries and 64 MiB unpacked at most; no nested archives, no paths leaving the archive, no links.
- Layout ids: `builtin:<name>`, name `[a-z]` then `[a-z0-9-]`, ending alphanumeric, 1-40 characters. Categories, in order: Welcome, Letters, Articles, Announcements, Products, Promotions, News, Guides, Events, Feedback.
- Brand slot ids: `logo`, `signature`, `follow`, `social`, `footer`, `reply`; signature children `signature-photo`, `signature-name`, `signature-title`; reply children `reply-heading`, `reply-button`, `reply-note`.
- Composition runs on `route:apps` at level `strong`, with `requestedVia: "email"`. It never falls back to local models. It never publishes.
- Description 1-8000 characters; template names at most 200 runes; a conflict copy is named `<name> (MemQL's version)`, truncated to fit.
- Brand kit picture warning above 512 KiB; contrast warns, never refuses; the footer must link `{{unsubscribeUrl}}`.
- Copy: no emoji; the extension word is "pack", never "plugin". Extension copy obeys `editors/productivity/test/copyLint.test.ts`: status lines at most 6 words, lines at most 25 words, no " -- ", never "Materializer", "Nexus" or "Campaigns" in user copy.
- UI: follow `clients/os/DESIGN.md` and `.agents/skills/memql-ui-design/SKILL.md` (the `frontend-design` skill is not available). Owner rules for every touched surface:
  - Clean and minimal, built from kit pieces.
  - No horizontal dividers in touched UI. Record lists lose their dividers in the shared style (Task 20).
  - No chips, badges, labels or toasts inside list items. A row's state stays the existing one-word state slot.
  - A page's sibling views use `LocalTabs`, as Fleet's machine page does.
  - State-changing acts live in the `ActionBar`.
  - Loading is skeleton shapes, never words.
  - Every touched surface is screenshotted in light and dark, at 1400 and 520 pixels.

## Review Focus

- **Same file names in different folders.** Every Fylo email folder has `email.html` and `email.txt`. Each reaches the composer as a distinct file; none overwrites another. Pinned in Task 11 (`TestReadArchiveKeepsSameNamesInDifferentFolders`) and Task 15 (`TestComposeContextNamesWorkspaceFilesDistinctly`).
- **The template changes while MemQL writes it.** Submit saves a copy beside it and says so, never overwriting. Pinned in Task 15 (`TestComposeSubmitSavesACopyWhenTheTemplateChanged`).
- **Cancel mid-run.** The session stops, the template reads failed ("Cancelled"), and a late submit is refused. Pinned in Task 17 (`TestCancelCompositionMarksFailedAndCancelsGoal`), Task 15 (`TestComposeSubmitRefusedUnlessWriting`) and Task 18 (`TestAppSessionStopsWhenItsRunIsCancelled`).
- **An account with no brand kit.** Write it is refused with a way forward; Start from the layout still works with the neutral look. Pinned in Task 14 (`TestCreateTemplateFromLayoutWithoutKitUsesNeutralLook`) and Task 17 (`TestComposeEmailRefusesWithoutBrandKit`).
- **The same picture used twice** (kit and content, or two blocks) counts once against the 8-picture budget. Pinned in Task 5 (`TestComposeIdenticalPicturesCountOnce`).

## Deviations from the spec, decided while planning

Each one comes from a fact found in the code. Task 27 adds a "Planning revisions" note to the spec listing them.

1. **Ask's layout choice runs in Ask's own turn.** A question asked inside a Claude Code session ends that session (`component/mcp/delegated_tools.go:275-284`); only the agent reply loop resumes after an answer (`integrations/agent/human_wait.go`). So Ask's agent lists layouts, asks with `requestUserFeedback` (`presentation: "layouts"`), then calls `campaignComposeEmail`, which therefore always takes a `layoutId`. Ask's suggestion reads the description, the file names and the catalogue; the pictures are seen by the composition run.
2. **ZIPs are unpacked on the server.** Members become Library files in a folder named `Email material`, archived after the run; the session pulls them as ordinary inputs. Cockpit is unchanged, so composing needs no Cockpit update on anyone's machine.
3. **Slot blocks are found by id anywhere in the document.** Confirmation's logo sits inside its coloured panel. The footer stays top level, as the renderer requires.
4. **The step submits a composition, never a whole document.** A composition is subject, preheader, content items, drops and repeats. The server rebuilds the email from the brand-applied layout, so brand pictures never pass through the model and the structure check is built in.
5. **Builtins beyond the spec's list:**
   - `campaignComposeContext`, `campaignComposeRender` and `campaignComposeSubmit` (the step's tool handlers);
   - `campaignRunComposition` (the run step);
   - `campaignCancelComposition` and `campaignRetryComposition`;
   - `campaignBrandPreview`;
   - `campaignLayoutCatalogue`.
6. **The New email page drops the per-file "Use in the email" switch.** The composer chooses among attached pictures; the separate picture of the finished email is guidance only.
7. **The step runs as a per-user system agent, `emailComposerAgent`** (role `specialist`), because a session's tools require an acting agent (`component/memql/app_session_provider.go:261-264`).
8. **The real-material acceptance moves to sub-project B.** That's composing the eleven Fylo emails and comparing with the fidelity harness, where Fylo's brand kit is created. A merges on the automated tests and the UI review.

---

## Phase 1: the email document

### Task 1: Hyphenated layout ids, catalogue metadata and slot names

**Files:**
- Modify: `component/compose/emaildoc/parse.go` (`builtinPattern`, ~:108)
- Modify: `component/compose/emaildoc/layouts.go`
- Modify: `component/compose/emaildoc/layouts_test.go`, `parse_test.go`, `gallery_fixtures_test.go` (:128)
- Modify: `component/campaigns/layout_test.go` (:735), `editors/productivity/test/email-document.test.ts` (:303, :951)

**Interfaces:**
- Produces:
  - `type RepeatRange struct{ Min, Max int }`.
  - `LayoutInfo` gains `Category string`, `Optional []string` and `Repeat map[string]RepeatRange`.
  - `func LayoutMeta(id string) (LayoutInfo, bool)` returns a copy.
  - `var LayoutCategories = []string{"Welcome", "Letters", "Articles", "Announcements", "Products", "Promotions", "News", "Guides", "Events", "Feedback"}`.
  - Slot constants `SlotLogo, SlotSignature, SlotFollow, SlotSocial, SlotFooter, SlotReply` (the ids in Global Constraints) and `func BrandSlots() []string` in that order.
  - Catalogue entries carry `category`, `optional` and `repeat`.

- [ ] **Step 1: Write the failing tests**

```go
func TestParseAcceptsHyphenatedBuiltinLayoutIDs(t *testing.T) {
	for _, id := range []string{"builtin:three-reasons", "builtin:a", "builtin:side-by-side"} {
		if !builtinPattern.MatchString(id) { t.Errorf("%s refused", id) }
	}
	for _, id := range []string{"builtin:Three", "builtin:-x", "builtin:x-", "builtin:" + strings.Repeat("a", 41)} {
		if builtinPattern.MatchString(id) { t.Errorf("%s accepted", id) }
	}
}

func TestLayoutCatalogueMetadata(t *testing.T) {
	for _, l := range Layouts() {
		if !slices.Contains(LayoutCategories, l.Category) { t.Errorf("%s: category %q", l.ID, l.Category) }
		for _, id := range l.Optional { requireContentBlock(t, l, id) }
		for id, r := range l.Repeat {
			requireContentBlock(t, l, id)
			if r.Min < 1 || r.Max < r.Min || r.Max > 6 { t.Errorf("%s: %s range %+v", l.ID, id, r) }
		}
		eachBlock(l.Document.Blocks, "", func(b Block, role string) {
			if slices.Contains(BrandSlots(), b.ID) && role != "brand" { t.Errorf("%s: slot %s is %s", l.ID, b.ID, role) }
		})
	}
}
```

`requireContentBlock` (a test helper in layouts_test.go) fails unless the id exists anywhere in the document with role content. Match `eachBlock`'s real signature in merge.go:84.

- [ ] **Step 2: Run them and confirm they fail.** Run `go test ./component/compose/emaildoc/ -run 'TestParseAcceptsHyphenated|TestLayoutCatalogueMetadata' -count=1`. Expected: FAIL (the pattern rejects hyphens; `Category` is undefined).
- [ ] **Step 3: Implement.**
  - Pattern: `^builtin:[a-z](?:[a-z0-9-]{0,38}[a-z0-9])?$`.
  - Existing entries: confirmation is Welcome, letter is Letters, article is Articles (optional `highlight`), spotlight is Announcements.
  - Replace the literal 4 in `gallery_fixtures_test.go` with `len(Layouts())`.
  - `component/campaigns/layout_test.go:735` compares against `emaildoc.Layouts()` ids.
  - The two TypeScript assertions count `fs.readdirSync` of `component/compose/emaildoc/layouts`.
- [ ] **Step 4: Run them and confirm they pass.** Run:
  - `go test ./component/compose/emaildoc/ -count=1`
  - `go test -count=1 ./component/campaigns/ -run Layout`
  - `cd editors/productivity && npm test`

  Expected: PASS.
- [ ] **Step 5: Commit** (`emaildoc: layout ids may hold hyphens, and the catalogue names categories, optional blocks and repeats`).

### Task 2: The four layouts on colour roles and brand slots

**Files:**
- Modify: `component/compose/emaildoc/layouts/{confirmation,letter,article,spotlight}.email.json`
- Modify: `component/compose/emaildoc/layouts_test.go` (`builtinTheme`, `TestLayoutsAreNeutral`, `builtinRoles`, `TestLayoutsSplitBrandAndContent`, `wantIssues`)
- Modify: `component/compose/emaildoc/merge_test.go` (:91, :97, :140, :142, :273, :282, :301, :315)
- Regenerate: `component/compose/emaildoc/testdata/golden/layout-*.{html,txt}`

**Interfaces:** Produces four layouts whose slots follow Global Constraints, used by Tasks 4-6 and 12.

- [ ] **Step 1: Write the failing test.** Set `builtinTheme` to the neutral grey theme:

  ```
  page #f1f1f1, canvas #ffffff, primary #2f2f2f, secondary #595959,
  accent #3d3d3d, tint #f5f5f5, text #1c1c1c, muted #666666,
  headingFont Georgia, bodyFont Arial
  ```

  Then:
  - remove Confirmation's canvas exception from `TestLayoutsAreNeutral`;
  - make `TestLayoutsSplitBrandAndContent` find `confirm` with a recursive `blockByID`;
  - rewrite `builtinRoles` for the new trees (below).

- [ ] **Step 2: Run it and confirm it fails.** Run `go test ./component/compose/emaildoc/ -count=1`. Expected: FAIL on the theme and roles.
- [ ] **Step 3: Rewrite the four JSON files.**
  - Every file uses the neutral theme.
  - **Confirmation:** wrap `top`, `logo`, `rule-top`, `headline`, `subhead`, `hero`, `confirm`, `note` and `rule-bottom` in a section `panel` with background `secondary`, white text kept; `footer` stays top level.
  - **Article:** replace `question`, `question-more` and `ask` with a brand section `reply` holding `reply-heading` (heading), `reply-button` (button) and `reply-note` (text).
  - **Spotlight:** drop `follow`.
  - **Letter:** `promo` and its children become content.

  Every file must still be exactly `Document.JSON()`. Move the merge tests that wrote to `ask` onto the `article` text block.
- [ ] **Step 4: Regenerate the goldens and check them.** Run:

  ```bash
  go test ./component/compose/emaildoc/ -run '^TestLayoutsGolden$' -update -count=1
  go test ./component/compose/emaildoc/ -count=1
  ```

  Expected: PASS, including `TestBuiltinLayoutsMeetTextContrast`.
- [ ] **Step 5: Commit** (`emaildoc: the four layouts draw colour roles on a neutral theme and share the brand slots`).

### Task 3: Contrast measurement in production code

**Files:**
- Create: `component/compose/emaildoc/contrast.go`
- Modify: `component/compose/emaildoc/layouts_contrast_test.go`

**Interfaces:**
- Produces:
  - `type TextRun struct{ Block, Words, Foreground, Background string; Size float64; Bold bool }`
  - `func (r TextRun) Large() bool`: true at 24px, or at 18.66px and bold.
  - `func (r TextRun) Required() float64`: 3 or 4.5.
  - `func ContrastRatio(a, b string) float64`
  - `func TextRuns(markup string) ([]TextRun, error)`

  Moved from the test file's `luminanceOf`, `contrastOf` and `textRuns`; it returns errors instead of taking `*testing.T`. Avoid the name `attr` (taken in blocks.go:1026).

- [ ] **Step 1: Write the failing test.**

```go
func TestContrastRatioKnownValues(t *testing.T) {
	if got := ContrastRatio("#ffffff", "#000000"); math.Abs(got-21) > 0.01 { t.Fatalf("white/black %v", got) }
	if got := ContrastRatio("#ffffff", "#55b9bd"); math.Abs(got-2.32) > 0.02 { t.Fatalf("white/teal %v", got) }
}
```

- [ ] **Step 2: Run it and confirm it fails.** Run `go test ./component/compose/emaildoc/ -run TestContrastRatioKnownValues -count=1`. Expected: FAIL (undefined).
- [ ] **Step 3: Move the helpers and switch the test file to them.**
- [ ] **Step 4: Run the package and confirm it passes.** Run `go test ./component/compose/emaildoc/ -count=1`. Expected: PASS.
- [ ] **Step 5: Commit** (`emaildoc: contrast measurement is package code, for brand warnings`).

### Task 4: Brand kits: parse, apply, warn

**Files:**
- Create: `component/compose/emaildoc/brand.go`, `brand_test.go`

**Interfaces:**
- Consumes: Tasks 1-3.
- Produces:

```go
type BrandPicture struct{ Src, Alt, Href string }
type BrandSignature struct{ Photo, NameImage, NameText, Title string }
type BrandFooter struct{ Lines []string; Links []Link; Separator *string }
type BrandReply struct{ Heading, ButtonLabel, ButtonHref, Note string }
type BrandKit struct {
	Theme     Theme
	Logo      BrandPicture
	Signature *BrandSignature
	Follow    string
	Social    []Link
	Footer    BrandFooter
	Reply     *BrandReply
}
func ParseBrandKit(data []byte) (BrandKit, error)
func (k BrandKit) JSON() ([]byte, error)
func ApplyBrand(layout Document, kit BrandKit) (Document, error)
type BrandWarning struct{ Code, Message string } // Code: "contrast" | "pictures_large"
func BrandWarnings(ctx context.Context, kit BrandKit) []BrandWarning
const BrandPicturesWarnBytes = 512 << 10
```

Canonical JSON keys, in order: `theme`, `logo{src,alt,href}`, `signature{photo,name{src|text},title}`, `follow`, `social[{network,href,icon}]`, `footer{lines,links[{label,href}],separator}`, `reply{heading,button{label,href},note}`.

`ParseBrandKit` rules:
- Unknown keys are refused, and the theme follows Parse's rules.
- `logo.src` is required.
- Every picture must be a `data:` picture that passes `pictureProblem`. `artifact:` is refused with "Upload pictures through the Brand page".
- Hrefs pass `hrefProblem`. Link tags are allowed only in footer links.
- The footer must hold a link whose href is `{{unsubscribeUrl}}`.
- Limits: 8 social links, 8 footer links, 12 footer lines of at most 300 characters, follow at most 200 characters.
- A signature name is a picture or text, exactly one.
- A reply needs a heading, a button label and a button href.

`ApplyBrand` rules:
- Replace the theme.
- Fill each slot found by id anywhere: logo; signature children; follow text; social links; footer lines, links and separator; reply children.
- A text signature name replaces the `signature-name` image with a text block of the same id in the heading font.
- Remove each slot the kit leaves empty.
- Return the result through `JSON()` and `Parse`.

`BrandWarnings`:
- Renders every built-in layout with the kit applied (`RenderOptions{Preview: true}`).
- Collects the `TextRuns` below `Required()` as one `contrast` warning per distinct (foreground, background, large) triple. Message: `Text in #ffffff on #55b9bd has a contrast of 2.3 to 1; the minimum here is 3 to 1.`
- Gives one `pictures_large` warning when the kit's distinct pictures decode to more than `BrandPicturesWarnBytes`.

- [ ] **Step 1: Write the failing tests:**
  - `TestBrandKitJSONRoundTrip`: `ParseBrandKit(kit.JSON())` deep-equals the kit, and `JSON` is stable.
  - `TestParseBrandKitRefusesFooterWithoutUnsubscribe`: the error mentions "unsubscribe".
  - `TestParseBrandKitRefusesArtifactPictures`.
  - `TestApplyBrandFillsEverySlot`, on `builtin:letter` and `builtin:article`. Asserts the logo src, `signature-photo` src, follow text, social links, footer links and reply children, and that the theme equals the kit's.
  - `TestApplyBrandRemovesEmptySlots`: a kit with no signature, reply, social or follow leaves none of those ids.
  - `TestApplyBrandSignatureNameAsText`: `signature-name` becomes `type: "text"` with the name.
  - `TestApplyBrandNeverTouchesContent`: every `ContentSlots` text is equal before and after.
  - `TestBrandWarningsReportsLowContrastWithoutRefusing`: secondary `#55b9bd`; parse succeeds; at least one `contrast` warning.
  - `TestBrandWarningsLargePictures`.

  Fixtures are tiny PNGs built in the test with `image/png` (no files).
- [ ] **Step 2: Run them and confirm they fail.** Run `go test ./component/compose/emaildoc/ -run 'Brand' -count=1`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement `brand.go`.**
- [ ] **Step 4: Run them and confirm they pass.** Run `go test ./component/compose/emaildoc/ -count=1`. Expected: PASS.
- [ ] **Step 5: Commit** (`emaildoc: a brand kit fills a layout's slots and warns, never refuses, on contrast`).

### Task 5: Composition operations

**Files:**
- Create: `component/compose/emaildoc/composition.go`, `composition_test.go`

**Interfaces:**
- Consumes: `Merge(layout Document, c Content, images map[string]string) (Document, error)`, `ContentSlots`, and `LayoutInfo` from Task 1.
- Produces:

```go
type Composition struct {
	Subject   string         `json:"subject"`
	Preheader string         `json:"preheader"`
	Drop      []string       `json:"drop"`
	Repeat    map[string]int `json:"repeat"`
	Content   []ContentItem  `json:"content"`
}
func Compose(layout Document, info LayoutInfo, c Composition, images map[string]string) (Document, error)
func ComposeSlots(layout Document, info LayoutInfo, drop []string, repeat map[string]int) ([]Slot, error)
func RepeatedID(id string, n int) string // n == 1 -> id; otherwise id + "-" + strconv.Itoa(n)
```

Rules:
- A drop names only `info.Optional`; otherwise the error is `the draft dropped a block this layout needs: <id>`.
- A repeat names only `info.Repeat`, with a count inside the range; otherwise the error is `the draft repeated <id> <n> times; this layout allows <min> to <max>`.
- A clone copies the block and every descendant, with ids from `RepeatedID`, inserted after the previous copy.
- Then `Merge`.

- [ ] **Step 1: Write the failing tests:**
  - `TestComposeDropsOnlyOptionalBlocks`
  - `TestComposeRepeatsWithinRangeAndSuffixesDescendants`: expects `step-2` and `step-title-2`, using a test layout and a test `LayoutInfo`.
  - `TestComposeRefusesRepeatOutsideRange`
  - `TestComposeLeavesBrandSlotsUntouched`: content aimed at `logo` is refused by `Merge`.
  - `TestComposeSlotsListsRepeatedIDs`
  - `TestComposeIdenticalPicturesCountOnce`: nine image blocks given the same data URI render with no `images_limit` issue.
  - `TestEveryLayoutStaysWithinPictureBudgetWithAFullKit`. For every `Layouts()` entry:
    1. Apply a full kit: logo, photo, name picture, three social icons, all distinct.
    2. Repeat every repeatable to its `Max`.
    3. Give every content image a distinct picture.
    4. Render.

    Then assert no `images_limit` issue.
- [ ] **Step 2: Run them and confirm they fail.** Run `go test ./component/compose/emaildoc/ -run 'Compose|PictureBudget' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement `composition.go`.**
- [ ] **Step 4: Run them and confirm they pass.** Run `go test ./component/compose/emaildoc/ -count=1`. Expected: PASS.
- [ ] **Step 5: Commit** (`emaildoc: a composition fills, drops and repeats only what its layout allows`).

### Task 6: Wireframes

**Files:**
- Create: `component/compose/emaildoc/wireframe.go`, `wireframe_test.go`
- Create (generated): `component/compose/emaildoc/testdata/golden/wireframe-<name>.svg`

**Interfaces:** Produces `func Wireframe(d Document) string`.

Drawing rules:
- `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 600 H" width="600" height="H">` on a white rect.
- Only these elements: `svg`, `rect`, `circle`, `line`, `g`.
- Only these colours: `#ffffff`, `#ececec` (picture fill), `#d6d6d6` (heading bars), `#e2e2e2` (text lines), `#c8c8c8` (outlines), `#bdbdbd` (buttons).
- Shapes per block type:

  | Block | Drawn as |
  |---|---|
  | image | A rect at its width and height (scaled to the column), with a diagonal picture mark |
  | heading | Bars, 0.6 × size high |
  | text | Three lines, 6px high, 10px apart, honouring alignment |
  | highlight | A light box with two lines |
  | button | A pill of radius 20 at its width, 40px high |
  | divider | A line |
  | spacer | Its height |
  | section | An outline around its children, with padding |
  | columns | Side by side by width percentage |
  | social | One circle of radius 10 per link, or 3 if none, on a 32px row |
  | footer | Five short centred lines |

- No `text` elements and no `image` elements.

- [ ] **Step 1: Write the failing tests:**
  - `TestWireframeUsesOnlyGreysAndShapes`: parse with `golang.org/x/net/html`; check tag and colour sets.
  - `TestWireframeIsDeterministic`
  - `TestLayoutsWireframeGolden`: `checkGolden(t, "wireframe-"+name+".svg", Wireframe(doc))` for each `Layouts()` entry.
- [ ] **Step 2: Run them and confirm they fail.** Run `go test ./component/compose/emaildoc/ -run Wireframe -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement, then generate the goldens.** Run `go test ./component/compose/emaildoc/ -run '^TestLayoutsWireframeGolden$' -update -count=1`. Open two of the SVGs and check by eye that they read as their layouts.
- [ ] **Step 4: Run the package and confirm it passes.** Run `go test ./component/compose/emaildoc/ -count=1`. Expected: PASS.
- [ ] **Step 5: Commit** (`emaildoc: every layout draws as a colourless wireframe`).

## Phase 2: the catalogue

**Shared rules for Tasks 7-10.**
- A layout is a neutral-theme `*.email.json` that is exactly `Document.JSON()`.
- It holds example words in the neutral "Acme" voice, never a client's copy or pictures.
- Pictures are empty placeholders with a `height`.
- Hrefs are empty or link tags.
- The footer links unsubscribe, preferences and view.
- The type scale follows the existing four: 552px content width; headings 48 / 26.7 / 21.3; body 14.7-16; buttons about 300px wide at size 20.
- The layouts the Fylo series needs take their measurements from the originals in `~/Downloads/fylo_email_marketing/STAGE B - N/email.html`. Read them; never copy anything from them into the repository.

**Each new layout adds:**
- the JSON file;
- a catalogue entry (id, name, description, category, optional, repeat);
- `builtinCatalog` subject and preheader;
- a `builtinRoles` string;
- a `wantIssues` entry;
- regenerated layout and wireframe goldens.

**Run after each task:**

```bash
go test ./component/compose/emaildoc/ -run '^(TestLayoutsGolden|TestLayoutsWireframeGolden)$' -update -count=1
go test ./component/compose/emaildoc/ -count=1
go test -count=1 ./component/campaigns/ -run Layout
cd editors/productivity && npm test
```

Expected: PASS, including contrast, picture budget, neutrality and the brand/content split.

Outlines below use `id:type`. Bracketed groups are children. `(brand)` marks a slot. "opt" means optional; `rep a..b` means repeatable in that range.

### Task 7: Lesson, Three reasons, Side by side (the Fylo series)

- **`builtin:lesson`** (Articles):
  - `logo:image(brand, 600x155 bleed)`, `banner:image(552x191)`
  - `body:section(tint)[intro:text, highlight:highlight opt, article:text]`
  - `rule:divider(brand)`
  - `promo:columns[promo-copy:section[promo-title:heading, promo-text:text], promo-picture:image(193x210)]`
  - `follow:heading(brand)`, `social:social(brand)`, `rule-2:divider(brand)`, `footer:footer(brand)`
  - Fylo: Stage B 2 and 5.
- **`builtin:three-reasons`** (Articles):
  - `logo`, `banner:image`
  - `icons:columns opt[icon-1:image(80x80), icon-2:image, icon-3:image]`
  - `body:section(tint)[lead:heading, reason:section rep 2..4[reason-title:text, reason-text:text], closing:highlight opt]`
  - `reply:section(brand)[reply-heading, reply-button, reply-note]`
  - `signature:columns(brand)[signature-photo, signature-name, signature-title]`
  - `rule:divider(brand)`, `footer`
  - Fylo: Stage B 8.
- **`builtin:side-by-side`** (Products):
  - `logo`, `banner:image`
  - `body:section(tint)[intro:text, statement:text]`
  - `cards:columns[card-a:section[card-a-icon:image(44x52), card-a-title:heading, card-a-subtitle:text, card-a-text:text], card-b:section[...]]`
  - `closing:section[closing-title:text, closing-text:text] opt`
  - `reply(brand)`, `signature(brand)`, `rule:divider(brand)`, `footer`
  - Fylo: Stage B 7.

- [ ] **Step 1: Add the three catalogue entries and test-list entries; run and confirm they fail** (missing files).
- [ ] **Step 2: Write the three JSON files.**
- [ ] **Step 3: Regenerate the goldens and run the commands above.** Expected: PASS.
- [ ] **Step 4: Commit** (`emaildoc: Lesson, Three reasons and Side by side layouts`).

### Task 8: Welcome tour, Plain note, Greeting, Announcement, Video

- **`builtin:welcome-tour`** (Welcome): `logo`, `hero:image(552x260)`, `headline:heading`, `intro:text`, `benefit:columns rep 1..3[benefit-icon:image(48x48), benefit-text:text]`, `cta:button`, `social(brand)`, `footer`.
- **`builtin:plain-note`** (Letters): `logo`, `letter:section(canvas)[greeting:heading, body:text]`, `signature(brand)`, `footer`.
- **`builtin:greeting`** (Letters): `logo`, `picture:image(600x400 bleed)`, `message:text`, `signature(brand)`, `footer`.
- **`builtin:announcement`** (Announcements): `logo`, `hero:image(552x300)`, `headline:heading`, `text:text`, `cta:button`, `social(brand)`, `footer`.
- **`builtin:video`** (Announcements): `logo`, `headline:heading`, `video:image(552x310, href empty)`, `text:text`, `cta:button`, `social(brand)`, `footer`.

- [ ] **Step 1: Add the entries; run and confirm they fail.**
- [ ] **Step 2: Write the five JSON files.**
- [ ] **Step 3: Regenerate and run.** Expected: PASS.
- [ ] **Step 4: Commit** (`emaildoc: Welcome tour, Plain note, Greeting, Announcement and Video layouts`).

### Task 9: Product launch, Product grid, Product focus, Offer, Newsletter

- **`builtin:product-launch`** (Products): `logo`, `product:image(552x360)`, `eyebrow:text opt`, `headline:heading`, `features:text`, `cta:button`, `social(brand)`, `footer`.
- **`builtin:product-grid`** (Products):
  - `logo`, `headline:heading`, `intro:text opt`
  - `row:columns rep 1..2[product-a:section[product-a-picture:image(260x260), product-a-name:heading, product-a-line:text, product-a-button:button], product-b:section[...]]`
  - `social(brand)`, `footer`
- **`builtin:product-focus`** (Products): `logo`, `focus:columns[focus-picture:image(260x300), focus-copy:section[focus-name:heading, focus-text:text, focus-button:button]]`, `quote:highlight opt`, `footer`.
- **`builtin:offer`** (Promotions): `logo`, `offer-headline:heading`, `offer-text:text`, `code:highlight`, `cta:button`, `fine-print:text opt`, `social(brand)`, `footer`.
- **`builtin:newsletter`** (News): `logo`, `intro:text`, `story:section rep 1..3[story-picture:image(552x240), story-title:heading, story-excerpt:text, story-link:button]`, `social(brand)`, `footer`.

- [ ] **Step 1: Add the entries; run and confirm they fail.**
- [ ] **Step 2: Write the five JSON files.**
- [ ] **Step 3: Regenerate and run.** Expected: PASS.
- [ ] **Step 4: Commit** (`emaildoc: Product launch, Product grid, Product focus, Offer and Newsletter layouts`).

### Task 10: Steps, Picture steps, Questions, Event, Testimonial, Review request, Thank you

- **`builtin:steps`** (Guides): `logo`, `headline:heading`, `intro:text`, `step:columns rep 2..6[step-number:heading, step-copy:section[step-title:text, step-text:text]]`, `cta:button opt`, `reply(brand)`, `footer`.
- **`builtin:picture-steps`** (Guides): `logo`, `headline:heading`, `row:columns rep 2..4[row-picture:image(200x150), row-text:text]`, `footer`.
- **`builtin:questions`** (Guides): `logo`, `headline:heading`, `intro:text opt`, `qa:section rep 2..6[qa-question:text, qa-answer:text]`, `reply(brand)`, `footer`.
- **`builtin:event`** (Events): `logo`, `picture:image(552x260)`, `headline:heading`, `details:section(tint)[details-when:text, details-where:text]`, `text:text opt`, `cta:button`, `footer`.
- **`builtin:testimonial`** (Feedback): `logo`, `quote:highlight`, `person:columns[person-photo:image(64x64), person-name:text]`, `cta:button`, `social(brand)`, `footer`.
- **`builtin:review-request`** (Feedback): `logo`, `headline:heading`, `product:image(300x300)`, `text:text`, `cta:button`, `footer`.
- **`builtin:thank-you`** (Feedback): `logo`, `headline:heading`, `text:text`, `tips:section(tint)[tips-title:heading, tips-text:text]`, `reply(brand)`, `signature(brand)`, `footer`.

- [ ] **Step 1: Add the entries; run and confirm they fail.**
- [ ] **Step 2: Write the seven JSON files.**
- [ ] **Step 3: Regenerate and run.** Expected: PASS, with 24 layouts.
- [ ] **Step 4: Update the catalogue descriptions.** `dsl/compose/builtins.memql:34` and the `emailLayouts` description in `dsl/campaigns/builtins.memql` say "the built-in layouts" instead of "the four". Run `make sdk-gen && make sdk-gen-check`.
- [ ] **Step 5: Commit** (`emaildoc: Steps, Picture steps, Questions, Event, Testimonial, Review request and Thank you layouts`).

## Phase 3: engine services

### Task 11: The archive reader, shared and parameterised

**Files:**
- Create: `integrations/compose/archive.go`, `archive_test.go`
- Modify: `integrations/compose/source_content.go` (`readReference`, `readReferenceMember`, `referenceBudget` use `ReadArchive`)

**Interfaces:**
- Produces:

```go
type ArchiveLimits struct {
	MaxEntries int; MaxUnpackedBytes int64; MaxTextBytes int64
	MaxImages int // 0 means no cap
	MaxPixels int64; SkipUnsupported bool
}
var MaterializerArchiveLimits = ArchiveLimits{MaxEntries: 64, MaxUnpackedBytes: 16 << 20, MaxTextBytes: 256 << 10, MaxImages: 8, MaxPixels: 24_000_000}
var CompositionArchiveLimits = ArchiveLimits{MaxEntries: 256, MaxUnpackedBytes: 64 << 20, MaxTextBytes: 4 << 20, MaxPixels: 24_000_000, SkipUnsupported: true}
type ArchiveMember struct{ Path, MIME string; Data []byte }
type SkippedMember struct{ Path, Reason string }
type Archive struct{ Members []ArchiveMember; Skipped []SkippedMember }
func ReadArchive(data []byte, limits ArchiveLimits) (Archive, error)
```

The Materializer keeps its numbers and its error sentences, now formatted from the limits.

- [ ] **Step 1: Write the failing tests:**
  - `TestReadArchiveCompositionLimits`: 256 entries accepted, 257 refused.
  - `TestReadArchiveRefusesPathsLeavingTheArchive`
  - `TestReadArchiveRefusesNestedArchives`
  - `TestReadArchiveRefusesLinks`
  - `TestReadArchiveSkipsUnsupportedWithReason`: an `.svg` and a `.pdf` are skipped and named, and the PNG is kept.
  - `TestReadArchiveKeepsSameNamesInDifferentFolders`: `a/email.txt` and `b/email.txt` both come back with their paths.
- [ ] **Step 2: Run them and confirm they fail.** Run `go test -count=1 ./integrations/compose/ -run Archive`. Expected: FAIL.
- [ ] **Step 3: Implement, and switch `source_content.go` to it.**
- [ ] **Step 4: Run them and confirm they pass.** Run `go test -count=1 ./integrations/compose/`. Expected: PASS; the existing source-content tests are unchanged.
- [ ] **Step 5: Commit** (`compose: one archive reader with limits per caller`).

### Task 12: Layout rows carry wireframe and category; the layout catalogue builtin

**Files:**
- Modify: `component/campaigns/layout.go` (built-in rows ~:256-258; organization rows)
- Modify: `component/campaigns/capabilities.go`
- Modify: `dsl/campaigns/builtins.memql`
- Test: `component/campaigns/layout_test.go`

**Interfaces:**
- Produces `emailLayouts` row keys `wireframe` (SVG) and `category`. Organization rows get `category: "Saved"` and a wireframe drawn from their document.
- Produces `campaignLayoutCatalogue` (executor `integration.campaigns.layoutCatalogue`, no args, `@sdk`). Rows are `{id, name, category, description, outline, wireframe}`, built-in only, where `outline` is block ids and types joined as in the plan's outlines, for the Ask agent.

- [ ] **Step 1: Write the failing tests:**
  - `TestEmailLayoutsRowsCarryWireframeAndCategory`: built-in rows' `wireframe` starts with `<svg`, and their category is in `emaildoc.LayoutCategories`.
  - `TestLayoutCatalogueListsBuiltInLayoutsWithOutlines`
- [ ] **Step 2: Run them and confirm they fail.** Run `go test -count=1 ./component/campaigns/ -run 'EmailLayouts|LayoutCatalogue'`. Expected: FAIL.
- [ ] **Step 3: Implement, and register `layoutCatalogue` in `Capabilities()`.**
- [ ] **Step 4: Regenerate the SDK and run the checks.** Run `make sdk-gen && make sdk-gen-check && go test -count=1 ./component/campaigns/ && go test -run TestEngineInitLoadsFullDSL ./component/memql/`. Expected: PASS.
- [ ] **Step 5: Commit** (`campaigns: layouts list their wireframe and category, and assistants can read the catalogue`).

### Task 13: Brand kit storage, save, preview

**Files:**
- Modify: `dsl/campaigns/concepts.memql` (concept `brandKit`)
- Modify: `dsl/campaigns/mutations.memql` (`createBrandKit`, `updateBrandKit`, both `@actor @serverOnly`)
- Modify: `dsl/campaigns/queries.memql` (`brandKitForAccount`)
- Modify: `dsl/campaigns/shapes.memql` (`brandKitFull`)
- Modify: `dsl/campaigns/builtins.memql` (`campaignSaveBrandKit`, `campaignBrandPreview`, both `@sdk`)
- Create: `component/campaigns/brandkit.go`, `brandkit_test.go`
- Modify: `component/campaigns/capabilities.go`
- Modify these engine lists:
  - `component/memql/organization_ownership.go:17-24` (`organizationOwnedConcept`)
  - `test/dslconformance/server_only_parsed_test.go:167` (want map)
  - `component/memql/rowauthz_enforce_gate_test.go:137` (`tierDecidesTheRead`, reason `memql#5598: ...`)
  - `component/node/routing.go:874-879`, with `routing_reach_test.go`
- Regenerate: `component/conceptfields/concept-fields.snapshot.json`, SDK

**Interfaces:**
- The concept `brandKit` has:
  - fields `ownerUserId string!`, `accountId string!`, `kit object!`;
  - `@serverWritten`, `@rowAuthz(owner="ownerUserId", clusterOwner, account="accountId")`;
  - the `// @no-displayCard:` reason "one configuration row per organization, shown on Email Settings";
  - the user and account relationships, as `testSettings` has them.
- Row id: `brandKitID(accountID) = hex(sha256("brand-kit:" + accountID))`.
- `campaignSaveBrandKit(accountId string!, kit object!, expectedRevision string)` returns `campaignBrandKitSaved {revision, warnings[{code, message}]}`.
- `campaignBrandPreview(accountId string!, kit object!, layoutId string)` returns `campaignBrandPreview {html, warnings}`, with `layoutId` defaulting to `builtin:article`.
- `func (w *Worker) brandKitFor(ctx context.Context, accountID string) (kit emaildoc.BrandKit, revision time.Time, ok bool, err error)`, used by Tasks 14-17.

Save, in order:
1. `requireOrganization`, then `mayWriteTemplates`.
2. Each `artifact:<id>` picture in the kit is read with `artifactImages{w}.ResolveImage` (under the caller) and replaced with a `data:` URI.
3. `emaildoc.ParseBrandKit`.
4. Revision check against the stored row (`errBrandKitChanged = errors.New("this brand changed; reload it before saving")`).
5. Write with the create or update mutation via `RenderCall`.
6. Return `emaildoc.BrandWarnings`.

- [ ] **Step 1: Write the failing tests** (fake engine, as in `template_test.go`):
  - `TestSaveBrandKitCreatesThenUpdatesWithRevision`
  - `TestSaveBrandKitRefusesAStaleRevision`
  - `TestSaveBrandKitStoresArtifactPicturesAsData`
  - `TestSaveBrandKitReturnsWarningsNotRefusals`
  - `TestSaveBrandKitRefusesWithoutWritePermission`
  - `TestSaveBrandKitRefusesFooterWithoutUnsubscribe`
  - `TestBrandPreviewRendersTheLayoutWithTheKit`
- [ ] **Step 2: Run them and confirm they fail.** Run `go test -count=1 ./component/campaigns/ -run 'BrandKit|BrandPreview'`. Expected: FAIL.
- [ ] **Step 3: Implement the DSL, the handlers and the engine-list entries.**
- [ ] **Step 4: Regenerate and run the guards.** Run:

  ```bash
  make concept-snapshot && make sdk-gen && make dsl-lint
  go test -count=1 ./component/campaigns/
  go test -run TestEngineInitLoadsFullDSL ./component/memql/
  go test -count=1 ./test/dslconformance/...
  go test -count=1 -run 'TestRowAuthzEnforcementLandGate|TierDecides' ./component/memql/
  go test -count=1 ./component/node/ -run Routing
  make concept-snapshot-check sdk-gen-check
  ```

  Expected: PASS.
- [ ] **Step 5: Commit** (`campaigns: an organization's brand kit, saved with warnings and previewed`).

### Task 14: A template's composition state, and creating one from a layout

**Files:**
- Modify: `dsl/campaigns/concepts.memql` (template field `composition object`)
- Modify: `dsl/campaigns/shapes.memql` (`templateSummary`, `templateFull`)
- Modify: `dsl/campaigns/mutations.memql` (`createTemplate`, `updateTemplate` accept `composition`)
- Modify: `dsl/campaigns/builtins.memql` (`campaignCreateTemplateFromLayout`, `@sdk`)
- Create: `component/campaigns/composition.go`, `template_from_layout.go`, `template_from_layout_test.go`
- Modify: `component/campaigns/template.go` (`values` carries composition when set), `template_test.go` (`keep()` field list), `capabilities.go`
- Regenerate: concept snapshot, SDK

**Interfaces:**
- The `composition` JSON:

  ```json
  {"goalId":"","state":"writing|done|failed","startedAt":"RFC3339","finishedAt":"RFC3339","error":"",
   "request":{"layoutId":"builtin:article","name":"","description":"","fileIds":[],"referenceFileId":""},
   "baseRevision":"RFC3339","notes":[]}
  ```

  `notes` lists, in plain sentences, each file left out and why (spec section 4, "named in the summary").

- Go types and functions:

```go
type compositionRequest struct{ LayoutID, Name, Description string; FileIDs []string; ReferenceFileID string }
type composition struct {
	GoalID, State, Error string
	StartedAt, FinishedAt, BaseRevision time.Time
	Request compositionRequest
	Notes   []string
}
func compositionFromRow(row map[string]any) (composition, bool)
func (w *Worker) writeComposition(ctx context.Context, templateID string, c composition) (revision time.Time, err error)
func (w *Worker) createTemplateFromLayout(ctx context.Context, accountID, name, layoutID string, c *composition) (templateID string, revision time.Time, issues []emaildoc.Issue, err error)
```

- `writeComposition` updates only the composition, under the template lock, and keeps the content.
- `campaignCreateTemplateFromLayout(accountId string!, name string!, layoutId string!)` returns `campaignTemplateSaved {templateId, revision, status, saved, issues}`.
  - It refuses a layout id that `emaildoc.LayoutMeta` doesn't know: `errUnknownLayout = errors.New("choose one of the built-in layouts")`.
  - It uses `brandKitFor`; with no kit, the neutral layout as is.
  - It calls `ApplyBrand` and saves a draft through the existing create path. The subject is the layout's example subject; the id is new, by `createTemplate`'s rules.

- [ ] **Step 1: Write the failing tests:**
  - `TestCreateTemplateFromLayoutAppliesTheBrandKit`
  - `TestCreateTemplateFromLayoutWithoutKitUsesNeutralLook`
  - `TestCreateTemplateFromLayoutRefusesUnknownLayout`
  - `TestCreateTemplateFromLayoutRefusesWithoutWritePermission`
  - `TestCompositionRoundTripsThroughTheTemplateRow`
  - `TestWriteCompositionKeepsTheContent`
- [ ] **Step 2: Run them and confirm they fail.** Run `go test -count=1 ./component/campaigns/ -run 'TemplateFromLayout|Composition'`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Regenerate and run the Task 13 guard list.** Expected: PASS.
- [ ] **Step 5: Commit** (`campaigns: a template remembers its composition, and one can be made from a layout at once`).

### Task 15: The composition step's tools, server side

**Files:**
- Modify: `dsl/campaigns/builtins.memql`. Three builtins, not `@sdk`:
  - `campaignComposeContext(templateId string!)`
  - `campaignComposeRender(templateId string!, composition object!)`
  - `campaignComposeSubmit(templateId string!, composition object!)`

  Executors: `integration.campaigns.composeContext`, `composeRender`, `composeSubmit`.
- Create: `dsl/campaigns/tools.memql`. Tools `emailComposeContext`, `emailComposeRender` and `emailComposeSubmit`:
  - `@handler(type="function", name="campaignCompose...")`, `@requiresAgentRole("specialist")`, `@executionTime("fast")`;
  - `templateId string @autoInjected`;
  - `composition object` on render and submit;
  - descriptions that teach the loop in Task 16's prompt.
- Create: `component/campaigns/compose_tools.go`, `compose_material.go`, `compose_tools_test.go`
- Modify: `component/campaigns/capabilities.go`

**Interfaces:**
- Material:

```go
type materialFile struct{ Workspace, MIME string; Data []byte; ArtifactID string }
type material struct {
	Files    []materialFile
	Handles  map[string]string // "memql-asset:<sha256>" -> data URI
	Skipped  []compose.SkippedMember
	Reference string           // workspace name of the picture of the finished email, or ""
}
func (w *Worker) compositionMaterial(ctx context.Context, req compositionRequest) (material, error)
```

  - It reads each file id under the caller.
  - A ZIP is read with `compose.ReadArchive(..., compose.CompositionArchiveLimits)`.
  - Workspace names:
    - a plain file keeps its Library name;
    - a ZIP member becomes `<zip name without .zip>__<path with "/" replaced by "__">`;
    - a duplicate gets `-2`, `-3`, and so on, before its extension.
  - Handles come from `compose.EmailAssetHandle`.
- `composeContext` answers the layout outline (id, type, role, sizes, example text), `optional`, `repeat`, `ComposeSlots`, the brand slots the kit fills, the pictures (`handle`, `file`, `mime`, `bytes`, `width`, `height`), the picture budget (8 and 1 MiB, less the brand's share), the request description, the reference file, and the account name.
- `composeRender` builds `Compose(ApplyBrand(layout, kit), info, c, material.Handles)`, then `emaildoc.Render` with preview. It answers `{subject, html, text, issues[{code, block, message}], slots}`.
- `composeSubmit`, in order:
  1. Refuse unless `composition.state == "writing"`: `errCompositionNotWriting = errors.New("This composition is no longer running.")`.
  2. Compose as render does, then save the draft through the template path with `expected = composition.BaseRevision`.
  3. On `errTemplateChanged`, create a new template named `<name> (MemQL's version)`, truncated to 200 runes, and mark the original's composition failed: "The template changed while MemQL was writing; its version is saved as <copy name>."
  4. Otherwise mark it `done`, recording in `notes` each skipped material file and its reason. For example: "Left out STAGE B - 1__readme.pdf: not a picture or text file."
  5. Answer `{templateId, revision, issues}`.

  Never publish.
- Every tool reads `templateId` only from the injected default; the caller must be the template's composition owner (`composition.request` belongs to its creator).

- [ ] **Step 1: Write the failing tests:**
  - `TestComposeContextListsPicturesWithHandles`
  - `TestComposeContextNamesWorkspaceFilesDistinctly`: two ZIP members `a/email.txt` and `b/email.txt`, plus a file named like one of them.
  - `TestComposeRenderReturnsIssues`
  - `TestComposeSubmitSavesDraftAndMarksDone`
  - `TestComposeSubmitSavesACopyWhenTheTemplateChanged`
  - `TestComposeSubmitRefusedUnlessWriting`
  - `TestComposeSubmitNeverPublishes`
  - `TestComposeToolsRefuseAnotherOwnersTemplate`
- [ ] **Step 2: Run them and confirm they fail.** Run `go test -count=1 ./component/campaigns/ -run Compose`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run the Task 13 guard list and confirm it passes.**
- [ ] **Step 5: Commit** (`campaigns: the composition step reads its material, renders drafts and submits`).

### Task 16: The run: agent, prompt, automation, session inputs

**Files:**
- Modify: `dsl/work/concepts.memql:40` (`requestedVia` gains `email`)
- Modify: `integrations/work/goal.go:313-318` (`validRequestedVia`)
- Modify: `dsl/work/builtins.memql:83`
- Modify: `integrations/work/operational_retention_policy_test.go:129-131`
- Create: `dsl/agents/emailComposerAgent.memql`:
  - `@scope("perUser")`, `seed agent emailComposerAgent`;
  - role `specialist`, roleSlug `system-email-composer`, kind `system`;
  - name "MemQL Email Composer";
  - modelled on `trainerAgent.memql`, with no providerConfig, so the run's route decides.
- Modify: `dsl/campaigns/prompts.memql`. Add `@level("strong") @templateFile("prompts/emailCompose.tmpl") prompt emailCompose { description string! context object! }`, and create `dsl/campaigns/prompts/emailCompose.tmpl`.
- Modify: `test/dslconformance/prompt_levels_test.go` (`emailCompose: "strong"`)
- Modify: `dsl/campaigns/automations.memql`. Add `@template automation composeEmailRun { args { templateId string! } run := builtin campaignRunComposition(templateId: args.templateId) }`.
- Modify: `dsl/campaigns/builtins.memql` (`campaignRunComposition(templateId string!)`, not `@sdk`, executor `integration.campaigns.runComposition`)
- Create: `component/memql/session_inputs.go`

```go
func WithSessionInputs(ctx context.Context, artifactIDs []string) context.Context
func SessionInputsFromContext(ctx context.Context) []string
```

- Modify: `component/memql/app_session_provider.go:301-316`. The handover sets `Inputs: SessionInputsFromContext(ctx)` when tools are offered.
- Create: `component/campaigns/compose_run.go`, `compose_run_test.go`
- Regenerate: concept snapshot, SDK

**Interfaces:**
- `handleRunComposition`:
  1. Load the template and its composition; return without action unless it's `writing`.
  2. `compositionMaterial`.
  3. Write each material file as a Library file owned by the composer, in folder `Email material` (create the folder if missing), and collect the artifact ids. Reuse the server-side Library writer the Materializer uses (`integrations/compose/materialize.go:1133-1191`) through a campaigns-declared seam wired in `app/`.
  4. Set up the context:
     - `memql.WithActingAgentId(ctx, "emailComposerAgent-"+owner)` and `memql.WithActingAgentRole(ctx, "specialist")`;
     - `common.ContextWithDelegatedToolDefaults(ctx, {"templateId": id})`;
     - `memql.WithSessionInputs(ctx, artifactIDs)`.
  5. Call `InvokeAIChatWithFilteredTools(ctx, "emailCompose", {"description": ..., "context": <composeContext answer>}, []string{"emailComposeContext", "emailComposeRender", "emailComposeSubmit"})`.
  6. Afterwards, archive the material files. If the composition is still `writing`, mark it failed: "MemQL finished without saving the email."; any error marks it failed with the error's sentence.
- The prompt says:
  - the rules in spec section 3.4;
  - the loop in spec section 3.3, with at most five renders;
  - copy from text files is used word for word;
  - the reference picture is guidance;
  - submit a composition (subject, preheader, content, drop, repeat), never a document;
  - pictures are referenced by handle;
  - never ask the person anything; the person is not present in this session.

- [ ] **Step 1: Prove the path.** Write `TestAppSessionHandoverCarriesSessionInputs` in `component/memql`: with tools offered, an acting agent, a matching run and inputs in context, the handover's `Inputs` equal them. Write `TestRunCompositionCallsTheComposerWithExactlyTheEmailTools` in `component/campaigns`: a fake engine records the prompt name, the tool names, the acting agent and role, the injected `templateId` and the session inputs.
- [ ] **Step 2: Run them and confirm they fail.** Run `go test -count=1 ./component/memql/ -run SessionInputs` and `go test -count=1 ./component/campaigns/ -run RunComposition`. Expected: FAIL.
- [ ] **Step 3: Implement.** Also write:
  - `TestRunCompositionMarksFailedWhenNothingIsSubmitted`
  - `TestRunCompositionArchivesItsMaterialFiles`
  - `TestRequestedViaAcceptsEmail` in `integrations/work`.
- [ ] **Step 4: Run them, the guards and the agent lane.** Run the Task 13 guard list, then `go test -tags agent -count=1 -timeout=300s ./integrations/agent/ ./integrations/agent/worker/` and `go test -count=1 ./integrations/work/`. Expected: PASS.
- [ ] **Step 5: Commit** (`campaigns: a composition runs as one Claude Code session with its material and three tools`).

### Task 17: Start, cancel and retry a composition

**Files:**
- Modify: `dsl/campaigns/builtins.memql`. All `@sdk`:
  - `campaignComposeEmail(accountId string!, name string!, layoutId string!, description string!, fileIds [string]!, referenceFileId string)`
  - `campaignCancelComposition(templateId string!)`
  - `campaignRetryComposition(templateId string!)`
- Create: `component/campaigns/compose_start.go`, `compose_start_test.go`
- Modify: `component/campaigns/worker.go`, which declares the seams below
- Create: `app/campaigns_composition.go`, `app/campaigns_composition_test.go`
- Modify: `app/engine.go:230-245`, which wires the seams

**Interfaces:**
- Seams:

```go
type CompositionGoal struct{ OwnerUserID, AccountID, TemplateID, Statement string }
type GoalStarter interface{ StartComposition(ctx context.Context, g CompositionGoal) (goalID string, err error) }
type GoalCanceller interface{ CancelGoal(ctx context.Context, goalID, reason string) error }
type AppReachability interface{ HasReachableApp(ctx context.Context, userID string) (bool, error) }
```

- The `app/` adapters:
  - **GoalStarter:** `memql.ParseRouteChoice("route:apps", "strong")` with `By = owner`, then `memql.ContextWithGoalRouteChoice`, then `work.OpenDirectGoal(work.DirectGoal{OwnerUserId, Statement, AutomationName: "composeEmailRun", RequestedVia: "email", AccountIds: {account}, Input: {"templateId": id}})`. The precedent is `integrations/work/model_catalog_research.go:31-38`.
  - **AppReachability:** any `Providers().AppDoors(ctx, user)` door that is `Reachable()`.
  - **GoalCanceller:** the work integration's cancel used by `cancelGoal`.
- `campaignComposeEmail`, in order:
  1. Validate: description 1-8000 characters; at most 64 file ids; name rules as `campaignSaveTemplate`.
  2. `requireOrganization`, then `mayWriteTemplates`.
  3. The account must have a kit: `errComposeNeedsBrand = errors.New("Set up this organization's brand first: Email, Settings, Brand.")`.
  4. An app must be reachable: `errComposeNeedsApp = errors.New("Open Cockpit with Claude Code signed in, then try again.")`.
  5. `createTemplateFromLayout` with composition `{state: writing, startedAt, request}`.
  6. `StartComposition`.
  7. `writeComposition` with `goalId` and `baseRevision` set to the revision that write returns.
  8. Answer `campaignCompositionStarted {templateId, goalId, revision}`.

  When the goal can't start, the composition is failed with the reason, and the template stays.
- `campaignCancelComposition`: when `writing`, call `CancelGoal(goalId, "cancelled by person")` and mark failed with "Cancelled".
- `campaignRetryComposition`: when `failed`, run the recorded request again with a new goal and state `writing`.

- [ ] **Step 1: Write the failing tests:**
  - `TestComposeEmailRefusesWithoutBrandKit`
  - `TestComposeEmailRefusesWithoutReachableApp`: asserts no template row is written.
  - `TestComposeEmailCreatesWritingTemplateAndStartsGoal`
  - `TestComposeEmailMarksFailedWhenGoalCannotStart`
  - `TestCancelCompositionMarksFailedAndCancelsGoal`
  - `TestRetryCompositionStartsNewGoalWithRecordedRequest`
  - `TestCompositionGoalStarterUsesAppsRouteChosenByOwner`, in `app/`: the context route choice is `route:apps` with `By == owner`, and `DirectGoal.AutomationName == "composeEmailRun"` with `RequestedVia == "email"`.
- [ ] **Step 2: Run them and confirm they fail.** Run `go test -count=1 ./component/campaigns/ -run 'ComposeEmail|CancelComposition|RetryComposition'` and `go test -tags agent -count=1 -timeout=900s ./app/ -run CompositionGoalStarter`. Expected: FAIL.
- [ ] **Step 3: Implement and wire.**
- [ ] **Step 4: Run them and the Task 13 guard list.** Expected: PASS.
- [ ] **Step 5: Commit** (`campaigns: start, cancel and retry a composition on the Apps route`).

### Task 18: Cancelling a run stops its app session

**Files:**
- Modify: the agent-side session delegate that opens a step's app session: `integrations/agent/worker/app_session_delegate.go` (find the opening call; the session context is the one `component/worker/session.go:222-237` cancels)
- Test: `integrations/agent/worker/app_session_delegate_test.go`

**Interfaces:** While a step's session runs, a watcher reads the run's cancel flag every 5 seconds, with the same read `component/automations/sequence.go:251-262` uses, and cancels the session context when it's set. It stops with the session.

- [ ] **Step 1: Write the failing test.** `TestAppSessionStopsWhenItsRunIsCancelled`: a fake run flips to cancel-requested, and the fake app sees its context cancelled within 6 seconds of a fake clock.
- [ ] **Step 2: Run it and confirm it fails.** Run `go test -tags agent -count=1 -timeout=300s ./integrations/agent/worker/ -run CancelledRun`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run it and confirm it passes.** Run `go test -tags agent -count=1 -timeout=300s ./integrations/agent/worker/`. Expected: PASS.
- [ ] **Step 5: Commit** (`agent: a cancelled run stops its app session`).

### Task 19: Ask chooses a layout, then starts the composition

**Files:**
- Modify: `dsl/agents/tools/requestUserFeedback.memql` (field `presentation enum("", "layouts")`)
- Modify: `integrations/work/human_feedback.go:18-48`. With `presentation: "layouts"`, every option value must be a built-in layout id, and the server adds `wireframe` (from `emaildoc.Wireframe`) to each option it stores. The model never supplies a wireframe.
- Modify: `component/memql/ask.go:55-64` (`AskQuestion` carries `Presentation`, and options keep `wireframe`)
- Modify: `dsl/campaigns/tools.memql`. Tools for Ask's assistant:
  - `emailLayoutCatalogue`: `@handler(type="function", name="campaignLayoutCatalogue")`, `@requiresAgentRole("assistant")`.
  - `startEmailComposition`: `@handler(type="function", name="campaignComposeEmail")`, `@requiresAgentRole("assistant")`.

  Their descriptions state the flow: list the layouts, pick the closest and two alternatives, ask with `requestUserFeedback` (kind choice, presentation layouts), then start the composition with the chosen id, the attachment file ids and the person's words.
- Tests: `integrations/work/human_feedback_test.go`, `component/memql/ask_test.go`

**Interfaces:** Produces `AskQuestion{..., Presentation string, Options []map[string]any /* label, value, wireframe? */}` for the OS (Task 25).

- [ ] **Step 1: Write the failing tests:**
  - `TestFeedbackLayoutsPresentationRequiresBuiltinLayoutIDs`
  - `TestFeedbackLayoutsPresentationAddsServerWireframes`: a model-supplied `wireframe` is dropped and replaced.
  - `TestAskQuestionCarriesPresentation`
- [ ] **Step 2: Run them and confirm they fail.** Run `go test -count=1 ./integrations/work/ -run Feedback` and `go test -count=1 ./component/memql/ -run AskQuestion`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run them and the Task 13 guard list.** Expected: PASS.
- [ ] **Step 5: Commit** (`ask: a choice of layouts shows their wireframes, and the assistant can start a composition`).

## Phase 4: clients

### Task 20: Record lists without dividers

**Files:**
- Modify: `clients/os/src/styles/index.css:1800-1802` (drop the `border-top` rule)
- Modify: `clients/os/src/kit/RecordRow.tsx` (header comment)
- Modify: `clients/os/DESIGN.md` (rule 1's divider sentences)
- Test: any test or QA note asserting list dividers (`grep -rn "os-line" clients/os/test clients/os/qa`)

**Interfaces:**
- DESIGN.md rule 1 reads: "Shared record lists draw no dividers. Rows are separated by their own spacing; a row shows the subtle hover and focus wash, and the open row keeps its accent edge."
- Rule 8's grouping-hairline guidance is unchanged.

- [ ] **Step 1: Write the failing test.** `test/kit/recordList.test.tsx`: "rows draw no border between them". Read the stylesheet text and assert no `border-top` rule targets `.os-record-row`, `.os-record-list > li + li` or `.os-livelist-row`.
- [ ] **Step 2: Run it and confirm it fails.** Run `cd clients/os && npx vitest run test/kit/recordList.test.tsx`. Expected: FAIL.
- [ ] **Step 3: Implement, and increase the row padding only if the screenshots crowd** (judge in Step 4).
- [ ] **Step 4: Run it, then screenshot.** Run `npx vitest run test/kit/recordList.test.tsx`. Expected: PASS. Screenshot the QA scenes `email-templates`, `deployables` and `fleet-machines` at 1400 and 520 pixels, light and dark (`npx vite --config qa/vite.config.ts --port 5199 --strictPort`, then `?view=...&mode=...`), and check that rows still read as separate items.
- [ ] **Step 5: Commit** (`os: record lists draw no dividers`).

### Task 21: VS Code New email: the wireframe grid and the two buttons

**Files:**
- Modify: `editors/productivity/src/newEmail.ts`, `newEmailPage.ts`, `newEmailView.ts`, `emailCopy.ts`
- Test: `editors/productivity/test/new-email.test.ts`, `new-email-view.test.ts`, `gallery/scenarios/newEmail.ts`
- Run first: `make sdk-gen && make vscode-deps`, so the builders for Tasks 12-17 exist

**Interfaces:**
- Consumes: `buildEmailLayouts`, `buildCampaignCreateTemplateFromLayout`, `buildCampaignComposeEmail` and `refusalSentence`.
- Produces the page messages:
  - page to host: `start{layoutId,name}`, `write{layoutId,name,description}`, `attachReference`, `removeReference{index}`, `attach`;
  - host to page: `setup{layouts:[{id,name,category,description,wireframe}]}`, `references`, `reference{item|null}`, `failed{problem}`, `idle`.

Behaviour:
- The grid shows `kind === "builtin"` rows only, grouped by category in `LayoutCategories` order.
- A card is the wireframe as `<img src="data:image/svg+xml;base64,...">` (the CSP allows `img-src data:`) with the name under it.
- Selection uses the accent underline and soft fill with straight edges (DESIGN rule 1). No dividers between groups; a group is a quiet heading.
- While `emailLayouts` is pending, skeleton cards keep the grid's geometry.
- **No `emailRender` calls.**
- Files are attached as today (64 files, 16 MiB, the same types). The "Use in the email" switch is removed. A separate optional "Picture of the finished email" takes one PNG, JPEG or GIF.
- **Start from the layout** calls `campaignCreateTemplateFromLayout`. **Write it** calls `campaignComposeEmail` with `fileIds` and `referenceFileId`. Both open `memql-file://<domain>/templates/<templateId>/<name>.email.json` with `memql.productivity.email` and close the page.
- A refusal shows as `failed{problem}`.
- Remove the `startMaterialize`, `followComposition` and `composeCancel` use, and the pending-request keys.
- Copy: "Picture of the finished email", "Start from the layout", "Write it". Status lines at most 6 words.

- [ ] **Step 1: Write the failing tests:**
  - `start creates the template from the layout and opens it`: asserts the call's args and the opened URI.
  - `write starts a composition with files and the reference picture`
  - `setup renders no email`: the fixture's `emailRender` is never called.
  - `the grid groups built-in layouts by category and draws wireframes`
  - `a refused write shows the server's sentence`

  Keep the 64-file, 16 MiB and unsupported-type tests.
- [ ] **Step 2: Run them and confirm they fail.** Run `cd editors/productivity && npm test`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run the tests, the compile and the gallery.** Run `npm test && npm run compile && npm run gallery`. Expected: PASS. Look at the New email scenario in both themes and check it against the UI rules.
- [ ] **Step 5: Commit** (`productivity: New email picks a layout from wireframes and creates the template at once`).

### Task 22: VS Code editor: the writing state, and no "Save as layout"

**Files:**
- Modify: `editors/productivity/src/documents.ts` (templates read `composition`)
- Modify: `editors/productivity/src/extension.ts` (`stat` is Readonly while writing)
- Modify: `editors/productivity/src/emailEditor.ts`, `emailPublish.ts`, `emailView.ts`, `emailCopy.ts`
- Test: `editors/productivity/test/email-publish.test.ts` (:202, :292-305), the editor tests

**Interfaces:**
- Consumes: `buildTemplateById`, `buildCampaignCancelComposition`, `buildCampaignRetryComposition`.
- While `composition.state === "writing"`:
  - the bar's state reads "MemQL is writing this email", with the elapsed time in `meta`;
  - the acts are Cancel only;
  - `edit` and `picture` messages are ignored;
  - the host polls `templateById` every 2 seconds, through `refreshQueue`, until the state changes, then reloads the document in place, as Markdown's `refreshRemote` does.
- When `failed`: the bar's state is the error sentence, and the acts are Retry plus the usual ones.
- `saveLayout` is removed from `editorBar`, `BarAct`, `ACT_WORDS`, `runAct` and the host.

- [ ] **Step 1: Write the failing tests:**
  - `a writing template shows the writing state and ignores edits`
  - `the editor reloads when the composition finishes`
  - `cancel calls campaignCancelComposition`
  - `a failed composition offers Retry`
  - `the bar never offers Save as layout`
- [ ] **Step 2: Run them and confirm they fail.** Run `cd editors/productivity && npm test`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run the tests and the compile.** Run `npm test && npm run compile`. Expected: PASS.
- [ ] **Step 5: Commit** (`productivity: the editor shows MemQL writing an email, and layouts are no longer saved`).

### Task 23: OS Templates: composition state, Retry and Cancel, no Layouts tab

**Files:**
- Modify: `clients/os/src/apps/email/rows.ts` (`TemplateRow.composition`, `templateFromRow`, `templateFingerprint`)
- Modify: `words.ts`
- Modify: `templates/TemplatesView.tsx`, `templates/TemplatePage.tsx`, `templates/templateActs.ts`
- Modify: `templates/TemplatesSection.tsx` (remove the Layouts view, `LocalTabs`, `onOpenLayout` and the TemplatePage layout link)
- Modify: `actions.ts` (`useCancelComposition`, `useRetryComposition` through `useWrite`)
- Modify: `test/email/harness.tsx` (new builtins), `test/email/templates/templateCreation.test.ts` (guard allows the two hooks)
- Test: `test/email/templates/list.test.tsx`, a new `templates/composition.test.tsx`; retire `layouts.test.tsx`
- QA: `qa/emailScenes.tsx` and `qa/emailSeeds.ts`. Add `email-templates` rows writing and failed, plus `email-template-writing` and `email-template-failed`. Remove the `email-layouts*` scenes.

**Interfaces:**
- **List row:** the state word is "Writing" (accent) while writing and "Failed" (warn, `stateTitle` = the error) when failed; otherwise as today. No new chips and no row actions.
- **Template page ActionBar** (rule 12):
  - writing: state "MemQL is writing this email", with the elapsed time in `meta`; acts Cancel.
  - failed: the error sentence; acts Retry, then the usual ones.
  - `composition.notes`, when present, appear once as a quiet `Notice` above the preview. They never appear in the list.

- [ ] **Step 1: Write the failing tests:**
  - `the list shows Writing for a template MemQL is writing`
  - `the list shows Failed with the reason on hover`
  - `the template page offers Cancel while writing`
  - `the template page offers Retry after a failure`
  - `Templates has no Layouts tab`
- [ ] **Step 2: Run them and confirm they fail.** Run `cd clients/os && npx vitest run test/email/templates`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run the tests and the typecheck, then screenshot.** Run `npx vitest run test/email && make os-typecheck`. Expected: PASS. Screenshot the new and changed QA scenes at 1400 and 520 pixels, light and dark, and check them against the UI rules.
- [ ] **Step 5: Commit** (`os: Templates show a composition being written, with Cancel and Retry where acts live`).

### Task 24: OS Email Settings: Brand

**Files:**
- Create: `clients/os/src/apps/email/settings/BrandPanel.tsx`, `settings/ColourField.tsx` (local to this panel; promote it on a second use)
- Modify: `settings/SettingsSection.tsx` (a Brand section after Senders)
- Modify: `useEmail.ts` (`useBrandKit(accountId)` via `brandKitForAccount`)
- Modify: `actions.ts` (`useSaveBrandKit`, `useBrandPreview`)
- Modify: `test/onePathCreate.test.ts` (ALLOWED, if the panel uploads)
- Test: `test/email/settings.test.tsx` (subjects gain "Brand"), new `test/email/settings/brand.test.tsx`
- QA: `email-settings-brand`, `email-settings-brand-warnings`

**Interfaces:**
- Consumes: `campaignSaveBrandKit`, `campaignBrandPreview`, `brandKitForAccount` and `uploads.upload`.
- Layout: `DetailsSection` and `SettingRow`, as on Fleet's machine Settings (`MachineDetailsInfo.tsx`). The heading sits beside the body on wide windows and stacks when narrow. **No dividers between fields.**
- Groups:
  - **Colours:** eight `ColourField`s. Each is an `<input type="color">` and a hex `Input` on one control line.
  - **Fonts:** heading and body `Select`s (Arial, Georgia, Helvetica, Inter, Sansita, Trebuchet MS, Verdana, Times New Roman), with a web-fonts `Switch`.
  - **Logo:** picture, alt text, link.
  - **Signature:** photo; name as a picture or as text; title.
  - **Follow line.**
  - **Social:** an editable collection of network, link and icon, with an Add control and removable entries.
  - **Footer:** lines and links. The preferences, unsubscribe and view links are present and their addresses can't be edited.
  - **Reply:** heading, button label, link, note.
- A live preview uses the existing `Preview` component with `campaignBrandPreview` on `builtin:article`, debounced 400 ms.
- Warnings show as a quiet `Notice` beside the preview.
- Save is the section's `Subhead` action, with the revision check. A refusal stays beside the section.
- Loading uses `ContentSkeleton`.

- [ ] **Step 1: Write the failing tests:**
  - `Settings lists Brand`
  - `saving sends the kit with the revision`
  - `contrast warnings show without blocking save`
  - `a refused save keeps the draft and shows the sentence`
  - `footer unsubscribe link cannot be removed`
- [ ] **Step 2: Run them and confirm they fail.** Run `cd clients/os && npx vitest run test/email/settings`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run the tests and the typecheck, then screenshot.** Run `npx vitest run test/email && make os-typecheck`. Expected: PASS. Screenshot `email-settings-brand` and `-warnings` at 1400 and 520 pixels, light and dark, and check them against the UI rules.
- [ ] **Step 5: Commit** (`os: Email settings hold each organization's brand, with a live preview`).

### Task 25: OS Ask: layout choices as wireframes

**Files:**
- Modify: `clients/os/src/ask/AskQuestion.tsx`, `ask/sdkTransport.ts` (carry `presentation` and the option `wireframe`)
- Test: `clients/os/test/ask/question.test.tsx`

**Interfaces:**
- With `presentation === "layouts"`, the options are a row of cards: the wireframe as `<img src="data:image/svg+xml;base64,...">` above its label, and the choice is answered with the option's value.
- Only an option's server-provided `wireframe` is drawn. Markdown messages still never render images.

- [ ] **Step 1: Write the failing tests:**
  - `layout choices draw their wireframes`
  - `a plain choice is unchanged`
- [ ] **Step 2: Run them and confirm they fail.** Run `cd clients/os && npx vitest run test/ask`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run the tests and the typecheck, then screenshot.** Run `npx vitest run test/ask && make os-typecheck`. Expected: PASS. Screenshot an Ask QA scene with a layout question, both modes.
- [ ] **Step 5: Commit** (`os: Ask shows a choice of layouts as wireframes`).

## Phase 5: retire and document

### Task 26: Emails leave the Materializer; documentation

**Files:**
- Modify: `integrations/compose/materialize.go` and `email_layout.go`. Retire `outputKind: "email_template"` with the refusal "Emails are made in the Email app."
- Modify: `app/materializer_composer.go` (remove `emailContentInstructions`, `emailContentSchema`, `composeEmail`), `dsl/compose/builtins.memql`
- Modify the Materializer client paths that offer an email: `clients/os/src/apps/materializer/make.ts:219-224`, `MakeWizard.tsx:123-124`
- Docs:
  - `docs/public/operate/campaign-sending.md`: new sections "Composing an email" and "Brand kits".
  - `docs/public/operate/memql-os.md`: Email: Brand settings, Templates writing state.
  - `docs/superpowers/specs/2026-10-11-materializer-front-door-design.md` and `2026-10-11-email-layouts-automations-design.md`: a top note pointing to `2026-11-04-email-composition-design.md` for emails.
  - Append "Planning revisions" (this plan's deviations list) to `docs/superpowers/specs/2026-11-04-email-composition-design.md`.
- Tests: `integrations/compose` and `app` tests that exercised email output, rewritten as refusal tests.

- [ ] **Step 1: Rewrite the email-output tests as refusal tests; run and confirm they fail.** Run `go test -count=1 ./integrations/compose/` and `go test -tags agent -count=1 -timeout=900s ./app/ -run Materializer`. Expected: FAIL.
- [ ] **Step 2: Remove the email path, then write the docs.**
- [ ] **Step 3: Run the tests and the docs gates.** Run the two commands above, then `make sdk-gen-check`, `go test -count=1 -run 'TestDocs|TestPluginMeansPackVocabulary' .` and `cd clients/os && npx vitest run test/materializer`. Expected: PASS.
- [ ] **Step 4: Commit** (`compose: emails are composed in the Email app, and the docs say how`).

## Phase 6: verify and merge

### Task 27: Full verification, UI review, merge into main

- [ ] **Step 1: Run every gate:**
  - `make test`
  - `make os-test`, `make os-typecheck`
  - `cd editors/productivity && npm ci && npm test && npm run compile`
  - `make sdk-gen-check concept-snapshot-check dsl-lint`
  - `go test -count=1 ./test/dslconformance/...`
  - `go test -count=1 -run TestAreaGraphIsADAG github.com/znasllc-io/memql`
  - `scripts/ci/module-boundaries.sh component/compose`
  - `make platform-graph`, then commit any regenerated graph.

  With a local Postgres (`docker ps` shows one), also run the database lane: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN='postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable' go test -count=1 ./component/campaigns/ ./integrations/work/ ./component/memql/`. Expected: all PASS. Fix anything red before going on.
- [ ] **Step 2: UI review.** Screenshot every surface touched in Tasks 20-25 at 1400 and 520 pixels, light and dark. Look at each against DESIGN.md and the owner's rules: no dividers, no chips in rows, acts in the bar, skeletons for loading. Fix and re-shoot until each passes.
- [ ] **Step 3: Merge and push.**

```bash
cd ~/projects/memql
git fetch origin
git checkout main && git pull --ff-only origin main
git merge --no-ff docs/email-composition-design -m "Merge email composition: wireframe layouts, brand kits and the Claude Code composition step"
make test
git push origin main
```

Expected: the merge is clean and `make test` passes on the merged tree before the push. If the merge conflicts, resolve it, run Step 1 again, then push.
