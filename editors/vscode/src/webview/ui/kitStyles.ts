// The stylesheet behind src/webview/ui/kit.ts: every `mq-*` class, and the
// re-statement of VS Code's own webview defaults in MemQL tokens.
//
// WHY A MODULE OF ITS OWN. brandTokens.ts folds this into brandStyleBlock(), so
// every panel document carries it under its CSP nonce with no per-panel
// wiring, and kit.ts re-exports it. kit.ts also READS brandTokens.ts (the
// mark), so keeping the CSS here is what keeps the two modules free of an
// import cycle.
//
// ONLY TOKENS. Every colour below is a `--memql-*` custom property, a
// `currentColor`, or a `color-mix()` of one; the palette itself lives in
// palette.ts, and test/brandTokens.test.ts refuses a hex anywhere else. Metrics
// come from the tokens too (`--memql-control-h`, `--memql-radius`,
// `--memql-motion-dur`), so a control line, a corner and a transition mean
// the same thing on every page.
//
// VS CODE'S DEFAULTS ARE OVERRIDDEN, NOT TRUSTED. The editor injects a default
// stylesheet into every webview -- link colours, `code` chips, the focus
// outline, scrollbars -- that follows the EDITOR's theme. Under a forced
// `memql.appearance` those would be the one part of a page still wearing the
// other theme, so the first block below says each of them again in tokens.
//
// HIGH CONTRAST keeps VS Code's contrast border on every drawn shape (buttons,
// the switch track, the bar, skeletons), and no state on this sheet is carried
// by colour alone: a switch's state is its thumb position, a bar's is its
// fill, an action bar's is the word beside the dot.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

/** Hard contrast: the two classes VS Code stamps in either high-contrast theme. */
const HC = ":is(body.vscode-high-contrast, body.vscode-high-contrast-light)";

/**
 * The bar's width as 101 attribute rules.
 *
 * A CSP with a style nonce drops every inline `style` attribute, so a width
 * cannot be written onto the element; the runtime sets `data-percent` instead
 * and these rules turn it into a width, which the fill then transitions to.
 */
function barRules(): string {
  const rules: string[] = [];
  for (let percent = 0; percent <= 100; percent += 1) {
    rules.push(`  .mq-bar-fill[data-percent="${percent}"] { width: ${percent}%; }`);
  }
  return rules.join("\n");
}

/** Every `mq-*` rule, plus the token re-statement of VS Code's webview defaults. */
export function kitStyles(): string {
  return `
  /* ---- VS Code's injected defaults, said again in MemQL tokens ---- */
  body { scrollbar-color: var(--memql-border-strong) transparent; }
  a, a code { color: var(--memql-accent); text-decoration: none; }
  a:hover { color: var(--memql-accent); text-decoration: underline;
            text-underline-offset: 2px; }
  a:focus, input:focus, select:focus, textarea:focus {
    outline: 1px solid var(--memql-focus); outline-offset: -1px; }
  a:focus:not(:focus-visible) { outline: none; }
  code { font-family: var(--vscode-editor-font-family, ui-monospace, monospace);
         font-size: 0.923em; color: inherit; background: var(--memql-raised);
         padding: 1px 4px; border-radius: 3px; }
  pre code { padding: 0; background: none; }
  ::selection { background: color-mix(in srgb, var(--memql-accent) 30%, transparent); }
  ::-webkit-scrollbar-corner { background: transparent; }

  /* ---- the page: head and body scroll, the action bar is the floor ---- */
  body:has(> main.mq-page) { padding: 0; display: flex; flex-direction: column;
                             min-height: 100vh; }
  .mq-page { flex: 1 0 auto; box-sizing: border-box; padding: 20px 24px 32px;
             line-height: 1.45; }
  main.mq-page + footer[data-region="actions"] { position: sticky; bottom: 0; z-index: 2; }
  main.mq-page + footer[data-region="actions"]:empty { display: none; }
  .mq-sr { position: absolute; width: 1px; height: 1px; margin: -1px; padding: 0;
           overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap; border: 0; }

  /* ---- head ---- */
  .mq-head { margin: 0 0 22px; }
  .mq-head-back { margin: 0 0 6px; }
  .mq-head-row { display: flex; flex-wrap: wrap; align-items: baseline;
                 column-gap: 10px; row-gap: 4px; min-height: var(--memql-control-h); }
  .mq-title { margin: 0; font-size: 1.35em; font-weight: 600; line-height: 1.3;
              letter-spacing: 0; overflow-wrap: anywhere; min-width: 0; }
  .mq-head-meta { color: var(--memql-muted); font-variant-numeric: tabular-nums; }
  .mq-head-aside { margin-left: auto; display: flex; align-items: center; gap: 2px;
                   align-self: center; margin-right: -6px; }
  .mq-back { display: inline-flex; align-items: center; gap: 3px; height: 22px;
             padding: 0 6px 0 0; margin-left: -3px; font: inherit; font-size: 0.923em;
             color: var(--memql-muted); background: none; border: 0;
             border-radius: var(--memql-radius); cursor: pointer;
             transition: color var(--memql-motion-dur) var(--memql-motion-ease); }
  .mq-back:hover { color: var(--memql-fg); }

  /* ---- groups: the subhead is the only container language ---- */
  .mq-subhead { display: flex; align-items: baseline; gap: 8px; margin: 28px 0 10px;
                font-size: 1em; font-weight: 600; line-height: 1.4; }
  .mq-subhead:first-child { margin-top: 0; }
  .mq-subhead-meta { font-weight: 400; color: var(--memql-muted);
                     font-variant-numeric: tabular-nums; }

  /* ---- facts ---- */
  .mq-facts { display: grid; grid-template-columns: minmax(8em, max-content) minmax(0, 1fr);
              align-items: baseline; column-gap: 24px; row-gap: 7px;
              margin: 0; max-width: 80ch; }
  .mq-facts dt { color: var(--memql-muted); }
  .mq-facts dd { margin: 0; min-width: 0; overflow-wrap: anywhere; }
  .mq-facts dd[data-muted] { color: var(--memql-muted); }
  .mq-mono { font-family: var(--vscode-editor-font-family, ui-monospace, monospace);
             font-size: 0.923em; }
  .mq-none { color: var(--memql-subtle); }

  /* ---- buttons: one button per bar, the rest are text acts ---- */
  .mq-btn, .mq-textbtn { display: inline-flex; align-items: center; justify-content: center;
                         gap: 6px; box-sizing: border-box; height: var(--memql-control-h);
                         margin: 0; font: inherit; line-height: 1; white-space: nowrap;
                         border-radius: var(--memql-radius); cursor: pointer;
                         transition: background-color var(--memql-motion-dur) var(--memql-motion-ease),
                                     border-color var(--memql-motion-dur) var(--memql-motion-ease),
                                     color var(--memql-motion-dur) var(--memql-motion-ease),
                                     text-decoration-color var(--memql-motion-dur) var(--memql-motion-ease); }
  .mq-btn { padding: 0 12px; font-weight: 500; color: var(--memql-fg);
            background: var(--memql-surface); border: 1px solid var(--memql-border-strong); }
  .mq-btn:hover { border-color: var(--memql-subtle); }
  .mq-btn[data-tone="primary"] { color: var(--memql-on-accent); background: var(--memql-accent);
                                 border-color: var(--memql-accent); }
  .mq-btn[data-tone="primary"]:hover { color: var(--memql-on-accent-hover);
                                       background: var(--memql-accent-deep);
                                       border-color: var(--memql-accent-deep); }
  .mq-btn[data-tone="danger"] { color: var(--memql-on-accent); background: var(--memql-danger);
                                border-color: var(--memql-danger); }
  .mq-btn[data-tone="danger"]:hover {
    background: color-mix(in srgb, var(--memql-danger) 86%, var(--memql-fg));
    border-color: color-mix(in srgb, var(--memql-danger) 86%, var(--memql-fg)); }
  .mq-textbtn { padding: 0 6px; color: var(--memql-muted); background: none; border: 0;
                text-decoration: underline; text-decoration-color: transparent;
                text-underline-offset: 3px; }
  .mq-textbtn:hover { color: var(--memql-fg); text-decoration-color: currentColor; }
  :is(.mq-btn, .mq-textbtn)[aria-busy="true"] { cursor: progress; }
  :is(.mq-btn, .mq-textbtn, .mq-back, .mq-disclosure-toggle):focus-visible {
    outline: 1px solid var(--memql-focus); outline-offset: 2px; }
  .mq-spin { flex: none; box-sizing: border-box; width: 10px; height: 10px; border-radius: 50%;
             border: 1.5px solid currentColor; border-right-color: transparent;
             animation: mq-spin 0.8s linear infinite; }
  @keyframes mq-spin { to { transform: rotate(360deg); } }
  .mq-acts { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; }

  /* ---- the action bar: the state in words, then the legal acts ---- */
  .mq-actbar { display: flex; flex-wrap: wrap; align-items: center; column-gap: 16px;
               row-gap: 8px; box-sizing: border-box;
               min-height: calc(var(--memql-control-h) + 22px); padding: 10px 24px;
               border-top: 1px solid var(--memql-border); background: var(--memql-surface); }
  .mq-actbar-confirm { flex: 1 0 100%; margin: 0; line-height: 1.45; }
  .mq-actbar-state { flex: 1 1 200px; display: flex; align-items: center; gap: 8px; min-width: 0; }
  .mq-actbar-word { flex: none; font-weight: 600; white-space: nowrap; }
  .mq-actbar-detail { min-width: 0; color: var(--memql-muted); overflow: hidden;
                      text-overflow: ellipsis; white-space: nowrap; }
  .mq-actbar-acts { margin-left: auto; display: flex; flex-wrap: wrap; align-items: center;
                    justify-content: flex-end; gap: 4px 8px; }
  .mq-actbar-acts > .mq-textbtn:last-child { margin-right: -6px; }
  .mq-dot { flex: none; box-sizing: border-box; width: 8px; height: 8px; border-radius: 50%;
            background: var(--memql-subtle); }
  .mq-dot[data-tone="idle"] { background: transparent; border: 1.5px solid var(--memql-subtle); }
  .mq-dot[data-tone="live"] { background: var(--memql-ok);
    box-shadow: 0 0 0 3px color-mix(in srgb, var(--memql-ok) 22%, transparent); }
  .mq-dot[data-tone="warn"] { background: var(--memql-warn); }
  .mq-dot[data-tone="error"] { background: var(--memql-danger); }
  .mq-dot[data-tone="busy"] { width: 12px; height: 12px; background: none;
    border: 1.5px solid color-mix(in srgb, var(--memql-accent) 25%, transparent);
    border-top-color: var(--memql-accent); animation: mq-spin 0.8s linear infinite; }

  /* ---- forms ---- */
  .mq-field { display: flex; flex-direction: column; gap: 5px; max-width: 34rem; margin: 0 0 16px; }
  .mq-field-main { display: flex; flex-direction: column; gap: 5px; }
  .mq-field-label { color: var(--memql-fg); }
  .mq-field-hint, .mq-field-error { margin: 0; font-size: 0.923em; line-height: 1.4; }
  .mq-field-hint { color: var(--memql-muted); }
  .mq-field-error { color: var(--memql-danger); }
  .mq-input { box-sizing: border-box; width: 100%; height: var(--memql-control-h); margin: 0;
              padding: 0 8px; font: inherit; color: var(--memql-fg); background: var(--memql-surface);
              border: 1px solid var(--memql-border-strong); border-radius: var(--memql-radius);
              transition: border-color var(--memql-motion-dur) var(--memql-motion-ease); }
  .mq-input::placeholder { color: var(--memql-subtle); opacity: 1; }
  .mq-input:hover { border-color: var(--memql-subtle); }
  .mq-input:focus { outline: 1px solid var(--memql-focus); outline-offset: -1px;
                    border-color: var(--memql-focus); }
  .mq-input[aria-invalid="true"] { border-color: var(--memql-danger); }
  .mq-input[aria-invalid="true"]:focus { outline-color: var(--memql-danger); }

  /* ---- switches: a native checkbox, role=switch, the track drawn here ---- */
  .mq-switch { position: relative; display: flex; align-items: flex-start; gap: 10px;
               max-width: 72ch; padding: 5px 0; cursor: pointer; }
  .mq-switch-input { position: absolute; width: 1px; height: 1px; margin: 0; opacity: 0;
                     pointer-events: none; }
  .mq-switch-track { position: relative; flex: none; box-sizing: border-box; width: 30px;
                     height: 18px; margin-top: 1px; border-radius: 9px; background: var(--memql-subtle);
                     transition: background-color var(--memql-motion-dur) var(--memql-motion-ease); }
  .mq-switch-track::after { content: ""; position: absolute; top: 2px; left: 2px; width: 14px;
                            height: 14px; border-radius: 50%; background: var(--memql-on-accent-hover);
                            box-shadow: 0 1px 2px rgb(0 0 0 / 0.28);
                            transition: transform var(--memql-motion-dur) var(--memql-motion-ease); }
  .mq-switch-input:checked + .mq-switch-track { background: var(--memql-accent); }
  .mq-switch-input:checked + .mq-switch-track::after { transform: translateX(12px); }
  .mq-switch[data-tone="danger"] .mq-switch-input:checked + .mq-switch-track {
    background: var(--memql-danger); }
  .mq-switch-input:focus-visible + .mq-switch-track { outline: 1px solid var(--memql-focus);
                                                     outline-offset: 2px; }
  .mq-switch:has(.mq-switch-input:disabled) { cursor: default; opacity: 0.55; }
  .mq-switch-text { display: flex; flex-direction: column; gap: 1px; min-width: 0;
                    line-height: 20px; }
  .mq-switch-note { color: var(--memql-muted); font-size: 0.923em; line-height: 1.4; }
  .mq-switch + .mq-field { margin: 6px 0 12px 40px; }

  /* ---- notices ---- */
  .mq-notice { display: flex; align-items: flex-start; gap: 10px; box-sizing: border-box;
               max-width: 80ch; margin: 12px 0; padding: 10px 12px;
               border: 1px solid var(--memql-border); border-radius: var(--memql-radius);
               background: var(--memql-surface); }
  .mq-notice[data-tone="info"] { background: var(--memql-accent-subtle); border-color: transparent; }
  .mq-notice[data-tone="warn"] { background: var(--memql-warn-subtle); border-color: transparent; }
  .mq-notice[data-tone="error"] { background: var(--memql-danger-subtle); border-color: transparent; }
  .mq-notice-icon { flex: none; margin-top: 2px; }
  .mq-notice[data-tone="info"] .mq-notice-icon { color: var(--memql-accent); }
  .mq-notice[data-tone="warn"] .mq-notice-icon { color: var(--memql-warn); }
  .mq-notice[data-tone="error"] .mq-notice-icon { color: var(--memql-danger); }
  .mq-notice-body { flex: 1 1 auto; min-width: 0; display: flex; flex-direction: column; gap: 3px; }
  .mq-notice-line { margin: 0; font-weight: 500; }
  .mq-notice-next { margin: 0; color: var(--memql-muted); }
  .mq-notice-acts { display: flex; flex-wrap: wrap; gap: 4px; margin: 2px 0 -4px -6px; }

  /* ---- a command a person may copy or run ---- */
  .mq-code { display: flex; align-items: center; gap: 8px; box-sizing: border-box;
             max-width: 80ch; margin: 6px 0 0; padding: 0 4px 0 10px;
             min-height: calc(var(--memql-control-h) + 6px);
             border: 1px solid var(--memql-border); border-radius: var(--memql-radius);
             background: var(--memql-surface); }
  .mq-code-text { flex: 1 1 auto; min-width: 0; overflow-x: auto; padding: 6px 0;
                  white-space: pre; background: none; border-radius: 0; color: var(--memql-fg); }

  /* ---- disclosure ---- */
  .mq-disclosure { margin: 20px 0 0; }
  .mq-disclosure-head { display: flex; align-items: center; gap: 10px; min-height: var(--memql-control-h); }
  .mq-disclosure-toggle { display: inline-flex; align-items: center; gap: 5px; height: var(--memql-control-h);
                          margin-left: -2px; padding: 0 4px 0 0; font: inherit; color: var(--memql-muted);
                          background: none; border: 0; border-radius: var(--memql-radius); cursor: pointer;
                          transition: color var(--memql-motion-dur) var(--memql-motion-ease); }
  .mq-disclosure-toggle:hover, .mq-disclosure-toggle[aria-expanded="true"] { color: var(--memql-fg); }
  .mq-chevron { flex: none; transition: transform var(--memql-motion-dur) var(--memql-motion-ease); }
  .mq-disclosure-toggle[aria-expanded="true"] .mq-chevron { transform: rotate(90deg); }
  .mq-disclosure-toggle[aria-expanded="true"] [data-when="closed"],
  .mq-disclosure-toggle[aria-expanded="false"] [data-when="open"] { display: none; }
  .mq-disclosure-meta { color: var(--memql-muted); font-variant-numeric: tabular-nums; }
  .mq-disclosure-body { margin-top: 6px; }
  .mq-disclosure-body[hidden] { display: none; }
  /* A log's own acts (Copy, Open in Output) ride up onto the toggle's line
     when the log is what the disclosure holds: one row, not two. */
  .mq-disclosure-body > .mq-logbox:first-child > .mq-log-head {
    width: fit-content; margin: calc(-6px - var(--memql-control-h)) -6px 6px auto; }

  /* ---- the live log ---- */
  .mq-logbox { min-width: 0; }
  .mq-log-head { display: flex; justify-content: flex-end; gap: 2px; margin: -4px -6px 2px 0; }
  .mq-log { box-sizing: border-box; max-height: 45vh; min-height: 5.5em; overflow: auto;
            margin: 0; padding: 8px 12px; border: 1px solid var(--memql-border);
            border-radius: var(--memql-radius); background: var(--memql-surface);
            font-family: var(--vscode-editor-font-family, ui-monospace, monospace);
            font-size: 0.923em; line-height: 1.55; overflow-wrap: anywhere; tab-size: 4; }
  .mq-log:focus-visible { outline: 1px solid var(--memql-focus); outline-offset: -1px; }
  .mq-log:empty::before { content: attr(data-empty); color: var(--memql-muted);
                          font-family: var(--vscode-font-family); }
  .mq-log-label { color: var(--memql-muted); }
  .mq-log-label::after { content: "  "; white-space: pre; }
  .mq-log-text { white-space: pre-wrap; }
  .mq-log-line[data-tone="error"] .mq-log-text { color: var(--memql-danger); }
  .mq-log-line[data-tone="muted"] .mq-log-text { color: var(--memql-muted); }

  /* ---- one progress screen for every long operation ---- */
  .mq-progress { display: flex; flex-direction: column; align-items: center; gap: 8px;
                 box-sizing: border-box; width: 100%; max-width: 420px; margin: 0 auto;
                 padding: 44px 0 16px; text-align: center; }
  .mq-progress-mark { color: var(--memql-accent); line-height: 0; margin-bottom: 10px; }
  .mq-progress-mark .memql-mark { display: block; }
  .mq-progress-title { margin: 0; font-size: 1.25em; font-weight: 600; line-height: 1.3; }
  .mq-bar { position: relative; box-sizing: border-box; width: 100%; height: 4px;
            margin: 10px 0 4px; border-radius: 2px; background: var(--memql-border); overflow: hidden; }
  .mq-bar-fill { height: 100%; width: 0; border-radius: inherit; background: var(--memql-accent);
                 transition: width calc(var(--memql-motion-dur) * 2.5) var(--memql-motion-ease),
                             background-color var(--memql-motion-dur) var(--memql-motion-ease); }
  .mq-bar[data-state="failed"] .mq-bar-fill { background: var(--memql-danger); }
  .mq-bar[data-state="stopping"] .mq-bar-fill { background: var(--memql-subtle); }
  .mq-bar[data-indeterminate] .mq-bar-fill { position: absolute; top: 0; left: 0; width: 32%;
                                             animation: mq-indeterminate 1.6s ease-in-out infinite; }
  @keyframes mq-indeterminate { from { transform: translateX(-100%); } to { transform: translateX(320%); } }
  .mq-progress[data-state="done"] .mq-bar { display: none; }
  .mq-progress-status { margin: 0; min-height: 1.45em; font-weight: 500; }
  .mq-progress[data-state="failed"] .mq-progress-status { color: var(--memql-danger); }
  .mq-progress-meta { margin: 0; min-height: 1.45em; color: var(--memql-muted);
                      font-size: 0.923em; font-variant-numeric: tabular-nums; }
  /* The progress screen is one centred column: what follows the block (the
     reason, the log) sits under it on the same axis, not at the page edge. */
  .mq-progress ~ :is(.mq-notice, .mq-disclosure, .mq-empty, .mq-code) {
    max-width: 600px; margin-left: auto; margin-right: auto; }
${barRules()}

  /* ---- loading is the shape of the content ---- */
  .mq-skeleton { position: relative; }
  .mq-skel { display: block; height: 10px; border-radius: 3px;
             background: color-mix(in srgb, var(--memql-border) 55%, var(--memql-border-strong));
             animation: mq-skel 1.6s ease-in-out infinite; }
  .mq-skel[data-w="xs"] { width: 3.5em; }
  .mq-skel[data-w="s"] { width: 6.5em; }
  .mq-skel[data-w="m"] { width: 11em; }
  .mq-skel[data-w="l"] { width: 16em; }
  .mq-skel[data-w="xl"] { width: 22em; }
  .mq-skel[data-w="full"] { width: 100%; }
  .mq-skel[data-h="title"] { height: 16px; }
  .mq-skel[data-h="control"] { height: var(--memql-control-h); border-radius: var(--memql-radius); }
  @keyframes mq-skel { 50% { opacity: 0.5; } }
  .mq-skel-facts { display: grid; grid-template-columns: minmax(8em, max-content) minmax(0, 1fr);
                   column-gap: 24px; row-gap: 13px; max-width: 80ch; padding: 3px 0; }
  .mq-skel-list > div { display: flex; flex-direction: column; gap: 8px; padding: 12px 0; }
  .mq-skel-list > div + div { border-top: 1px solid var(--memql-border); }
  .mq-skel-form > div { display: flex; flex-direction: column; gap: 8px; max-width: 34rem;
                        margin: 0 0 16px; }
  .mq-skel-head { display: flex; flex-direction: column; gap: 10px; margin: 0 0 26px; }
  .mq-skel-sub { margin: 28px 0 12px; }
  .mq-skel .mq-skel, .mq-skel-facts .mq-skel { max-width: 100%; }

  /* ---- a real empty state, after a successful empty read ---- */
  .mq-empty { display: flex; flex-direction: column; align-items: flex-start; gap: 10px;
              padding: 20px 0; }
  .mq-empty-line { margin: 0; color: var(--memql-muted); }

  /* ---- narrow panes ---- */
  @media (max-width: 560px) {
    .mq-page { padding: 16px 16px 28px; }
    .mq-actbar { padding: 10px 16px; }
  }
  @media (max-width: 400px) {
    .mq-facts, .mq-skel-facts { grid-template-columns: minmax(0, 1fr); row-gap: 1px; }
    .mq-facts dd { margin-bottom: 9px; }
    .mq-skel-facts { row-gap: 8px; }
  }

  /* ---- reduced motion: the fact stays, the movement goes ---- */
  @media (prefers-reduced-motion: reduce) {
    .mq-spin, .mq-dot[data-tone="busy"], .mq-skel { animation: none; }
    .mq-dot[data-tone="busy"] { border-color: var(--memql-accent); }
    .mq-bar[data-indeterminate] .mq-bar-fill { animation: none; width: 100%; opacity: 0.35; }
  }

  /* ---- high contrast: VS Code's border on every drawn shape ---- */
  ${HC} :is(.mq-btn, .mq-input, .mq-notice, .mq-code, .mq-log) {
    border-color: var(--vscode-contrastBorder, var(--memql-border-strong)); }
  ${HC} .mq-actbar { border-top-color: var(--vscode-contrastBorder, var(--memql-border-strong)); }
  ${HC} :is(.mq-switch-track, .mq-bar, .mq-dot) {
    outline: 1px solid var(--vscode-contrastBorder, var(--memql-border-strong)); outline-offset: 0; }
  ${HC} .mq-bar { background: transparent; }
  ${HC} .mq-input[aria-invalid="true"] { border-color: var(--memql-danger); }
  ${HC} .mq-skel { background: none;
    outline: 1px dashed var(--vscode-contrastBorder, var(--memql-border-strong)); }
  ${HC} :is(.mq-textbtn, .mq-back, .mq-disclosure-toggle):hover {
    outline: 1px dashed var(--vscode-contrastActiveBorder, var(--memql-focus)); outline-offset: -1px; }
`;
}
