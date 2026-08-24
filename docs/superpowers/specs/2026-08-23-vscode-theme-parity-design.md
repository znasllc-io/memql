# VS Code extension: portal theme parity -- an appearance setting, and MemQL editor themes

- **Date:** 2026-08-23
- **Status:** approved (autonomous run: recommendations selected by Claude at the owner's standing instruction)
- **Sub-project K** of the 2026-08-23 backlog brief (VS Code + install/release batch)
- **Owner ask:** "the VSCode plugin needs to have the same theme, color, look and feel
  as the portal for all the views; currently some views follow the theme set by
  VS Code but I would like to provide the two color themes that are available in
  the portal: light, dark and system; there are different views following
  different colors and I don't want that."

> **Correction (2026-08-24, during implementation).** The panel count below is
> wrong, and how it got wrong is worth more than the number. There are **seven**
> `*Panel.ts` files and **nine** panel classes -- `automationPanel.ts` hosts
> `AutomationRunPanel` + `StepTracePanel`, and `runPanel.ts` hosts `RunPanel` +
> `ResultPanel`. `runPanel.ts` is the file this record omits entirely, and it
> was already inlining `brandStyleBlock()`, so the conclusions below are
> unaffected -- only the census is.
>
> **The cause was two raw NUL bytes** in `runPanel.ts`'s `panelKey()` template
> literal. A NUL makes a file binary to the toolchain: `grep` skips it in
> SILENCE (no match, no "binary file matches" line, exit 1), `file` reports
> "data", and `git diff` prints "Binary files differ". The survey that produced
> this record used grep, so the seventh panel was invisible to it. The wrong
> count then travelled into all four task issues before anyone re-derived it.
>
> Fixed in memql#4422: the bytes are escapes now, two more offenders elsewhere
> in the package were found by a byte-level sweep, and
> `editors/vscode/test/sourceBytes.test.ts` fails the build on any raw NUL. The
> original text is left below rather than edited, because a design record that
> silently self-corrects is one nobody can audit.

## Current state (verified 2026-08-23)

- `editors/vscode/src/webview/brandTokens.ts` (memql#4196) already carries the
  portal palette (`memql#4177`'s exact hexes) as `--memql-*` custom properties.
  Light is the default; `body.vscode-dark` flips to dark; both high-contrast
  classes remap every token onto VS Code's own variables. All six webview
  panels (`conceptPanel`, `constructPanel`, `deploymentPanel`,
  `automationPanel`, `connectionPanel`, `addClusterPanel`) inline
  `brandStyleBlock()` under their CSP nonce; the three `*Screens.ts` modules
  are pure HTML-fragment producers rendered inside those panels.
- **The palette selection is bound to the EDITOR's theme.** VS Code stamps
  `vscode-dark`/`vscode-light` on the webview body; there is no user-facing
  MemQL appearance choice. An operator running a light editor cannot have dark
  MemQL panels, or vice versa.
- **Native surfaces cannot be extension-styled.** Tree rows, the activity bar,
  view titles and the status bar are drawn by the workbench and accept only
  `ThemeIcon` + `ThemeColor` (`brandTokens.ts:21-24` records this). This is a
  VS Code platform constraint, not a MemQL gap.
- `package.json` contributes **no color themes** (`"themes": []` absent), and
  no `memql.*` appearance setting exists (only `memql.lsp.*`).
- The repo-root guard `brand_shared_source_test.go` scans only
  `clients/portal/src` and `component/identity/web`; `editors/vscode` is
  deliberately outside it, and the inline palette there is established
  precedent (memql#4196's header states the CSP reasons).

So the user-visible inconsistency has two independent causes: (a) the panels
follow the editor rather than a MemQL choice, and (b) the native chrome around
them can never be extension-themed, so it always follows the editor. Fixing
(a) alone would make (b) MORE visible. The design addresses both.

## Decisions

### D1 -- `memql.appearance`: light | dark | system (default system)

A single extension setting, `memql.appearance`, enum `"system" | "light" |
"dark"`, default `"system"`.

- `system` means **follow the editor's theme kind** -- inside VS Code, "system"
  is the editor (which itself may follow the OS). This matches what the portal's
  "system" means relative to ITS host (the browser/OS).
- `light` / `dark` force the MemQL palette in every MemQL webview regardless of
  the editor theme.
- **High contrast always wins.** When the editor is in either high-contrast
  mode, the panels keep deferring wholesale to `--vscode-*` variables exactly as
  today (`brandTokens.ts:73-90`); the setting is ignored. A themed green is not
  worth an accessibility regression -- that sentence is already in the file and
  it stays true under the override.

### D2 -- Mechanism: the host resolves, the token module selects on a stamped attribute

`brandStyleBlock()` currently keys on `body.vscode-dark`. It changes to key on
`body[data-memql-theme="dark"]`, and a small host-side resolver decides what to
stamp:

- New pure module `src/webview/appearance.ts` (vscode-free, like every state
  module -- `cmd/memql-lsp/vscodeimportrule_test.go` applies):
  `effectiveTheme(setting, editorKind) -> "light" | "dark" | "hc"` with the
  rules of D1. Unit-tested under bare `node --test`.
- Each panel host reads `memql.appearance` + `vscode.window.activeColorTheme.kind`,
  calls `effectiveTheme`, and passes the result into its HTML builder, which
  stamps `<body data-memql-theme="...">`. In the `hc` case nothing is stamped
  and the existing `vscode-high-contrast*` classes carry the deferral.
- Panels re-render on `vscode.window.onDidChangeActiveColorTheme` and on
  `workspace.onDidChangeConfiguration` for `memql.appearance`. Panels already
  re-render HTML wholesale on every message (the state modules document this),
  so a repaint is the established pattern -- no incremental CSS swapping.

One resolver, stamped once per render, selected on in one stylesheet. No panel
may read the setting directly; the host passes the resolved value down, so the
screens modules stay vscode-free.

### D3 -- Ship "MemQL Dark" and "MemQL Light" editor color themes

The only way the native chrome (trees, activity bar, tabs, status bar) can wear
the brand is for VS Code's OWN theme to be a MemQL theme. So the extension
contributes two full workbench color themes generated from the same palette:

- `contributes.themes`: `MemQL Dark` (`uiTheme: "vs-dark"`) and `MemQL Light`
  (`uiTheme: "vs"`), files `themes/memql-dark-color-theme.json` +
  `themes/memql-light-color-theme.json`, committed (a VSIX packs files, not
  build steps).
- **Single palette source.** Extract the hex palette out of `brandTokens.ts`
  into `src/webview/palette.ts` (pure data: the two `--memql-*` value maps).
  `brandTokens.ts` composes its CSS from it; a generator script
  (`editors/vscode/scripts/generate-themes.mjs`, run by hand when the palette
  changes) emits the two theme JSONs from it; and a `node --test` drift test
  (`test/themes.test.ts`) regenerates in memory and fails when the committed
  JSON disagrees with `palette.ts`. Same shape as the repo's other
  generate-then-gate pairs (`make frontdoor` / `frontdoor-*-check`).
- **Scope of the theme files:** workbench colors only, mapped from the palette
  (editor background/foreground, sideBar, activityBar, statusBar, titleBar,
  list rows + selection, buttons, badges, inputs, focusBorder, panel borders,
  terminal ANSI left at defaults) plus a MINIMAL `tokenColors` set derived from
  the data tints (`--memql-data-number`, `--memql-data-string`, accent for
  keywords). This is a branded workbench, not a hand-tuned syntax theme; a full
  syntax pass is out of scope and the spec says so deliberately.
- The themes are OPT-IN (the user picks them in VS Code's theme picker). The
  extension never changes the user's editor theme itself -- but see D4.

### D4 -- One-time gentle offer, never a takeover

On first activation after this feature lands, if the current editor theme is
not a MemQL theme, the extension shows one information message: "MemQL panels
follow your editor theme. For the full MemQL look including the sidebar, switch
to the MemQL Dark or MemQL Light theme." with actions [Switch] [Not now].
Recorded in `globalState` so it fires at most once ever. No nagging, no
auto-switching -- changing a user's editor theme unasked is hostile.

### D5 -- A coverage gate so no panel ships off-brand

New `node --test` test asserting every `src/webview/*Panel.ts` html builder
inlines `brandStyleBlock()` (import-graph or output-string check). Today all
six do; the gate is what keeps the seventh honest. The three `*Screens.ts`
fragment modules are exempt by construction (they render inside a panel).

> **Correction (2026-08-24):** "all six ... the seventh" carries the miscount
> the box at the top of this record explains; it is seven files and nine panel
> classes, and all nine were already compliant. The decision is unaffected --
> if anything the real numbers strengthen it, because the two files hosting TWO
> panels each are exactly where a per-file check passes while one document
> ships bare. That is why the implemented gate's unit is the DOCUMENT rather
> than the file, and why the `*Screens.ts` exemption is asserted structurally
> (those modules build no document) rather than granted by filename.

## What this does NOT change

- Tree rows keep the `ThemeIcon` + `ThemeColor` (`charts.*`) vocabulary --
  platform constraint, documented in `brandTokens.ts:21-24`, unchanged.
- The CSP stance (no bundled fonts, no external stylesheets, inline under
  nonce) is unchanged; the two-voice font split
  (`--vscode-font-family` / `--vscode-editor-font-family`) is unchanged.
- The portal itself is untouched. If the portal palette changes, the update
  path is: edit `palette.ts`, run the generator, commit both -- the drift test
  enforces the pairing.

## Testing

- `node --test` (extension `dist-test` lane): `effectiveTheme` truth table
  (3 settings x 4 editor kinds); palette/theme-JSON drift test; panel coverage
  gate; a render test that `data-memql-theme` lands on body for forced light
  under a dark editor.
- Manual checklist in the task: flip setting live with a panel open (repaint,
  no reload); high-contrast editor ignores the setting; theme picker shows both
  MemQL themes; trees legible in both.

## Out of scope

- Restyling native tree rows (impossible by platform design).
- A full syntax highlighting theme beyond the minimal tokenColors set.
- Importing `brand/` css files into the extension build (the guard doesn't
  require it and the CSP/story in memql#4196 stands; revisit only if the
  palette starts drifting in practice).
- Cockpit (terminal) theming -- different repo, different rendering model.

## Risks

- Palette drift between portal and extension remains possible at the moment
  the portal changes (the drift gate binds extension-internal copies to each
  other, not to `brand/`). Mitigation: the palette module carries a header
  naming `brand/` as upstream and memql#4177 as the source of the hexes, and
  the release checklist line "portal palette changed? regenerate editor
  themes" lands in `editors/vscode/README.md`.
- VS Code may add theme kinds; `effectiveTheme` maps unknown kinds to `dark`
  (the safer contrast direction) and the truth-table test pins it.

## Task breakdown (preview; tasks carry the acceptance criteria)

1. `palette.ts` extraction + `brandStyleBlock` re-keyed on `data-memql-theme`
   + `effectiveTheme` resolver + panel host wiring + live repaint.
2. `memql.appearance` setting contribution + docs.
3. Theme generator + two committed theme JSONs + `contributes.themes` + drift
   test + one-time offer (D4).
4. Coverage gate (D5) + README notes + manual checklist run.
