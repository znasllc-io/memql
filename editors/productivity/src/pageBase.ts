// What the two email pages (the New email page and the email editor) share: the
// icons, the content security policy, the document shell and the base styles. The
// look is the Markdown editor's, read from the same theme variables, so the pages
// sit beside it as siblings.
//
// Pure: no vscode and no DOM. A view script may import ICONS from here, so this
// module must take nothing heavy with it (it does not use markdown.ts's
// escapeHTML, which would bring markdown-it into the view bundle).

const icon = (path: string) => `<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${path}</svg>`;

/** Inline 16 pixel line icons in the current colour, drawn as markdownPage.ts draws its own. */
export const ICONS: Readonly<Record<"design" | "source" | "desktop" | "mobile" | "plain" | "up" | "down" | "plus" | "trash" | "attach" | "mic" | "close" | "picture", string>> = {
  design: icon('<rect x="3" y="4" width="18" height="16" rx="2"/><path d="M3 10h18M10 10v10"/>'),
  source: icon('<path d="m8 6-6 6 6 6m8-12 6 6-6 6m-3-14-2 16"/>'),
  desktop: icon('<rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4"/>'),
  mobile: icon('<rect x="7" y="2" width="10" height="20" rx="2"/><path d="M11 18h2"/>'),
  plain: icon('<path d="M4 6h16M4 10h16M4 14h10M4 18h13"/>'),
  up: icon('<path d="M12 19V5m-6 6 6-6 6 6"/>'),
  down: icon('<path d="M12 5v14m-6-6 6 6 6-6"/>'),
  plus: icon('<path d="M12 5v14M5 12h14"/>'),
  trash: icon('<path d="M3 6h18M9 6V4h6v2M5 6l1 14h12l1-14M10 10v6M14 10v6"/>'),
  attach: icon('<path d="m8 13 7-7a3 3 0 0 1 4 4l-9 9a5 5 0 0 1-7-7l9-9m-7 12 9-9"/>'),
  mic: icon('<rect x="9" y="2" width="6" height="12" rx="3"/><path d="M5 10v2a7 7 0 0 0 14 0v-2M12 19v3M8 22h8"/>'),
  close: icon('<path d="m6 6 12 12M6 18 18 6"/>'),
  picture: icon('<rect x="3" y="4" width="18" height="16" rx="2"/><circle cx="9" cy="9.5" r="1.6"/><path d="m3 17 5-5 4 4 3-3 6 6"/>'),
};

export const escape = (value: string) => value.replace(/[&<>"']/g, character => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[character]!));

/**
 * The policy of an email page: its own nonce-marked script, inline styles,
 * embedded pictures and same-origin frames (the preview), and nothing else. The
 * nonce is a random token made by the host, never anything the email supplies.
 */
export function pageCSP(nonce: string): string {
  if (!/^[A-Za-z0-9+/_=-]+$/.test(nonce)) throw new Error("A page nonce must be a plain token.");
  return `default-src 'none'; script-src 'nonce-${nonce}'; style-src 'unsafe-inline'; img-src data:; frame-src 'self'; form-action 'none'; base-uri 'none'`;
}

/**
 * The whole document of a page: the policy, the styles, the page's markup and then
 * its script (`script` is the script's address). The title, the script address and
 * the body class are escaped; `styles` and `body` are the page's own markup and are
 * written as they are.
 */
export function pageShell(options: { title: string; nonce: string; script: string; styles: string; body: string; bodyClass?: string }): string {
  const policy = pageCSP(options.nonce);
  const bodyClass = options.bodyClass ? ` class="${escape(options.bodyClass)}"` : "";
  return `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta http-equiv="Content-Security-Policy" content="${policy}"><title>${escape(options.title)}</title><style>${options.styles}</style></head><body${bodyClass}>${options.body}<script nonce="${options.nonce}" src="${escape(options.script)}"></script></body></html>`;
}

// The base styles. The values are the Markdown page's, so the three read as siblings:
// 34 pixel controls with a 6 pixel radius, the filled `.primary` button, a 2 pixel
// focus outline, the `.views` segmented control and the skeleton's tone and sweep.
//
// What the pages put in their markup:
//   - Buttons: plain buttons are quiet; `.primary` is the one filled button of a view,
//     `.secondary` has a hairline, `.text-act` is a link-coloured act, `.icon-button`
//     is a square icon. A toggle shows `aria-pressed="true"`.
//   - `.views`: a group of buttons, one pressed (`aria-pressed` or, in a radio group,
//     `aria-checked`). Labelled buttons lose their icons on a narrow window.
//   - `.bar`: the action bar, sticky at the bottom: `.bar-state` (add `.busy` for the
//     spinner) on the left and `.bar-acts` on the right.
//   - `.notice`: a quiet line of words; `role="alert"` makes it a problem.
//   - `.subhead`: a group title. Disclosures are plain details and summary.
//   - `.switch`: a label around `<input type="checkbox" role="switch">` and its words.
//   - `.sk`: a grey placeholder block, and the shapes skeletonHTML makes. They stand
//     still until the element holding them (or one around it) has `.loading`.
//   - `.sr-only`: words for a screen reader alone.
const TONE = "color-mix(in srgb,var(--vscode-editor-foreground) 10%,var(--vscode-editor-background))";

export const BASE_STYLES = [
  // Reset and type. VS Code's own webview stylesheet pads the body by 20 pixels at the sides; the pages run edge to edge.
  `*{box-sizing:border-box}body{margin:0;padding:0;color:var(--vscode-editor-foreground);background:var(--vscode-editor-background);font:var(--vscode-font-size,13px)/1.6 var(--vscode-font-family,system-ui)}[hidden]{display:none!important}button,input,select,textarea{font:inherit}`,
  // Buttons.
  `button{display:inline-flex;align-items:center;justify-content:center;gap:7px;min-height:34px;padding:6px 12px;border:1px solid transparent;border-radius:6px;color:inherit;background:transparent;cursor:pointer}button:hover{background:var(--vscode-toolbar-hoverBackground)}button:disabled{opacity:.45;cursor:default}button svg{flex-shrink:0}button[aria-pressed="true"]{background:var(--vscode-toolbar-activeBackground,var(--vscode-list-inactiveSelectionBackground))}`,
  `.icon-button{padding:8px;width:34px;height:34px}.primary{color:var(--vscode-button-foreground);background:var(--vscode-button-background);border-color:var(--vscode-button-border,transparent)}.primary:hover{background:var(--vscode-button-hoverBackground)}.secondary{border-color:var(--vscode-panel-border)}.text-act{padding:6px 8px;color:var(--vscode-textLink-foreground)}.text-act:hover{background:transparent;text-decoration:underline}`,
  // Fields. The zero-specificity :where() lets a page restyle a field with one class.
  `:where(input:not([type=checkbox],[type=radio],[type=color],[type=file],[type=range]),select,textarea){display:block;width:100%;min-height:34px;padding:6px 10px;color:var(--vscode-input-foreground);background:var(--vscode-input-background);border:1px solid var(--vscode-input-border,var(--vscode-panel-border));border-radius:6px}textarea{resize:vertical;min-height:98px;padding:10px 12px;line-height:1.6}::placeholder{color:var(--vscode-input-placeholderForeground)}`,
  // Focus: one outline for every stop a keyboard can reach: the controls, links, and the pages' own stops (anything with a
  // tabindex, an outline row, a radio, a switch). VS Code's own stylesheet comes first with `input:focus` and `a:focus`
  // (an element and a pseudo-class); each selector here is at least that specific, so coming after it, it wins.
  `a:focus-visible,button:focus-visible,input:focus-visible,select:focus-visible,summary:focus-visible,textarea:focus-visible,[tabindex]:focus-visible,[role="treeitem"]:focus-visible,[role="radio"]:focus-visible,[role="switch"]:focus-visible{outline:2px solid var(--vscode-focusBorder);outline-offset:2px}`,
  // The segmented control.
  `.views{display:inline-flex;gap:2px;padding:3px;border:1px solid var(--vscode-panel-border);border-radius:9px}.views button:not(.icon-button){min-width:75px}.views [aria-checked="true"]{background:var(--vscode-toolbar-activeBackground,var(--vscode-list-inactiveSelectionBackground))}`,
  // The action bar: state on the left, acts on the right.
  `.bar{position:sticky;bottom:0;z-index:4;display:flex;align-items:center;justify-content:space-between;gap:16px;min-height:48px;padding:0 16px;background:var(--vscode-editor-background);border-top:1px solid var(--vscode-panel-border)}.bar-state{flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:13px;color:var(--vscode-descriptionForeground)}.bar-state.busy::before{content:"";display:inline-block;width:10px;height:10px;margin-right:8px;vertical-align:-1px;border:1.5px solid currentColor;border-right-color:transparent;border-radius:50%;animation:spin 1s linear infinite}.bar-acts{display:flex;flex:none;align-items:center;gap:8px}`,
  // Notices, subheads and disclosures.
  `.notice{margin:0;font-size:12px;line-height:1.6;color:var(--vscode-descriptionForeground);overflow-wrap:anywhere}.notice[role="alert"]{color:var(--vscode-errorForeground)}.subhead{margin:0;font-size:14px;line-height:1.4;font-weight:600}summary{cursor:pointer;padding:7px 0}`,
  // The switch: a checkbox drawn as a 28 by 16 pixel track and knob.
  `.switch{display:inline-flex;align-items:center;gap:8px;min-height:34px;cursor:pointer}.switch input{appearance:none;-webkit-appearance:none;flex:none;width:28px;height:16px;margin:0;border:1px solid var(--vscode-panel-border);border-radius:10px;background:radial-gradient(circle at 7px 7px,var(--vscode-descriptionForeground) 4px,transparent 4.5px) var(--vscode-input-background);cursor:pointer}.switch input:checked{background:radial-gradient(circle at 19px 7px,var(--vscode-button-foreground) 4px,transparent 4.5px) var(--vscode-button-background)}`,
  // Words for a screen reader alone.
  `.sr-only{position:absolute;width:1px;height:1px;margin:-1px;padding:0;overflow:hidden;clip-path:inset(50%);white-space:nowrap;border:0}`,
  // Skeletons: a grey tone, a sweep across it while loading, and the shape of each block.
  `.sk,.sk-rule,.sk-social i{background:${TONE}}.sk{position:relative;overflow:hidden;border-radius:4px}.sk::after{content:"";position:absolute;inset:0;background:linear-gradient(100deg,transparent 20%,color-mix(in srgb,var(--vscode-editor-background) 65%,transparent) 50%,transparent 80%);transform:translateX(-100%)}.loading .sk::after{animation:sk-sweep 2.4s ease-in-out infinite}`,
  `.sk-email{display:flex;flex-direction:column;gap:12px;width:100%;max-width:600px;margin:0 auto;pointer-events:none}.sk-picture,.sk-button,.sk-center{margin-left:auto;margin-right:auto}.sk-right{margin-left:auto}.sk-heading{width:70%}.sk-line{height:14px}.sk-band{height:40px}.sk-button{height:44px;border-radius:22px}.sk-rule{height:1px}.sk-gap{flex:none}`,
  `.sk-section{display:flex;flex-direction:column;gap:12px;padding:20px;border-radius:6px;background:color-mix(in srgb,var(--vscode-editor-foreground) 4%,var(--vscode-editor-background))}.sk-columns{display:flex;gap:16px}.sk-columns>div{display:flex;flex-direction:column;gap:12px;min-width:0}`,
  `.sk-social{display:flex;justify-content:center;gap:12px}.sk-social i{display:block;width:32px;height:32px;border-radius:50%}.sk-footer{display:flex;flex-direction:column;align-items:center;gap:8px;padding-top:12px}.sk-footer .sk-line{width:50%}.sk-footer .sk-line:nth-child(2){width:36%}.sk-footer .sk-line:nth-child(3){width:44%}`,
  // A narrow window: labelled view buttons shrink and drop their icons.
  `@media(max-width:600px){.views button:not(.icon-button){min-width:0;padding:5px 8px}.views button svg:not(:only-child){display:none}}`,
  // Motion: the spinner turns, the sweep crosses, and neither moves for a reader who asked for less.
  `@keyframes spin{to{transform:rotate(360deg)}}@keyframes sk-sweep{to{transform:translateX(100%)}}@media(prefers-reduced-motion:reduce){*,*::before,*::after{scroll-behavior:auto!important;animation:none!important}}`,
].join("\n");
