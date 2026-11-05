// The New email page: the static markup and styles. The view script
// (newEmailView.ts) fills it from the host's messages; nothing here depends on
// the person's organizations or layouts, so the host builds it once per panel.
//
// Pure: no vscode and no DOM. Every word comes from EMAIL_COPY.

import { EMAIL_COPY } from "./emailCopy.js";
import { BASE_STYLES, ICONS, pageShell, escape } from "./pageBase.js";

const C = EMAIL_COPY.newEmail;


/** The longest name, in UTF-16 code units, as New Markdown counts it. */
const NAME_LIMIT = 150;
/** A slash or a backslash would make the name a folder path; a control character has no place in a file name. */
const NAME_REFUSED = /[\\/\u0000-\u001f]/;

/**
 * Why a name can't be the new email's, as the sentence the page shows, or
 * nothing when it can. The rule is New Markdown's (markdownNameProblem): words
 * after trimming, no slash, backslash or control character, at most 150
 * characters. A blank name is refused too; the page says that one in its
 * action bar, not under the field.
 */
export function emailNameProblem(value: string): string | undefined {
  return !value.trim() || NAME_REFUSED.test(value) || value.length > NAME_LIMIT ? C.nameInvalid : undefined;
}

/** How many card shapes stand in while the layouts load. */
/** The cards drawn in each of the loading groups. */
const PLACEHOLDERS = [5, 3, 4];

// Pure, so the view script, which takes the name rule from this module, leaves the styles behind.
const STYLES = /* @__PURE__ */ [
  // The frame: one column read top to bottom, left aligned, the action bar at the foot of the window.
  `body{display:flex;flex-direction:column;min-height:100vh}.page{flex:1 0 auto;width:100%;max-width:960px;padding:40px 40px 48px}`,
  `.head{margin:0 0 28px}.title{margin:0;font-size:24px;line-height:1.3;font-weight:600;letter-spacing:-.025em}.meta{margin:4px 0 0;font-size:13px;color:var(--vscode-descriptionForeground)}.line{font-size:13px}`,
  // The form: groups 24 pixels apart, labels above their fields, no boxes.
  `.form{display:flex;flex-direction:column;gap:24px}.field{display:flex;flex-direction:column;gap:6px;min-width:0}.field>label,.label{font-size:12px;line-height:1.5;color:var(--vscode-descriptionForeground)}.optional{margin-left:6px}.narrow{max-width:420px}.notice:empty{display:none}#description{min-height:120px}`,
  // The layout cards. Each is a soft tinted mat (theme colours, no border, no hard-coded colour) round the server's drawing, with the
  // name directly under it. Every drawing sits in the same portrait frame (about 4:5) with object-fit: contain, anchored top and centre:
  // a tall layout is scaled down until it fits whole, a short one keeps the full width at the top and the mat shows below it. So every
  // card is the same size, no layout is cropped and nothing is padded with white page. A group is a quiet heading with its cards.
  // The chosen card's mat takes the soft accent fill and its name the accent colour and underline, with straight edges.
  `#layout-group{gap:0}#groups:empty{display:none}.layouts{display:flex;flex-direction:column;gap:28px}.set{display:flex;flex-direction:column;gap:12px}.set-head{margin:0;font-size:12px;line-height:1.5;color:var(--vscode-descriptionForeground)}`,
  `.cards{display:grid;grid-template-columns:repeat(auto-fill,minmax(150px,1fr));gap:16px}.card{display:flex;flex-direction:column;gap:6px;min-width:0;cursor:pointer;border-radius:0}.card:focus-visible{outline-offset:4px}`,
  `.thumb,.tile{aspect-ratio:4/5;overflow:hidden;padding:9px;border-radius:0;background:color-mix(in srgb,var(--vscode-editor-foreground) 6%,transparent)}.thumb img{display:block;width:100%;height:100%;object-fit:contain;object-position:top center}`,
  `.card:hover .thumb{background:color-mix(in srgb,var(--vscode-editor-foreground) 10%,transparent)}.card[aria-checked="true"] .thumb{background:color-mix(in srgb,var(--vscode-focusBorder) 24%,transparent)}`,
  `.card-name{padding:2px 0 4px;overflow:hidden;font-size:13px;line-height:1.4;text-overflow:ellipsis;white-space:nowrap;box-shadow:inset 0 -2px 0 transparent}.card:hover .card-name{box-shadow:inset 0 -2px 0 var(--vscode-descriptionForeground)}`,
  `.card[aria-checked="true"] .card-name{color:var(--vscode-textLink-foreground);box-shadow:inset 0 -2px 0 var(--vscode-focusBorder)}`,
  // The server draws a white page with grey panels. On a dark editor that is a glaring block, so it is drawn inverted: the page sits just
  // above the mat (a partial invert, so white lands on a dark grey) and the panels and lines read lighter than it. One small block, named for what it does; the OS Ask page uses the same rule.
  `body.vscode-dark .thumb img,body.vscode-high-contrast:not(.vscode-high-contrast-light) .thumb img{filter:invert(.82)}`,
  // Loading has the loaded page's geometry: a quiet heading bar, then a row of card-shaped mats, in a few groups. No words.
  `.loading-group{display:flex;flex-direction:column;gap:12px}.loading-group+.loading-group{margin-top:28px}.placeholder{display:flex;flex-direction:column;gap:6px}.tile{padding:0}.heading-bar{width:72px;height:12px}.placeholder .sk-line{width:60%;height:14px}`,
  // The description's tools (attach and dictate, as in the Markdown composer) and the references after them.
  `.tools{display:flex;flex-wrap:wrap;align-items:flex-start;gap:8px}.tool{flex:none;border-color:var(--vscode-contrastBorder,color-mix(in srgb,var(--vscode-descriptionForeground) 65%,transparent))}.tool:hover{border-color:var(--vscode-editor-foreground)}`,
  `.chips{display:flex;flex:1 1 0;flex-wrap:wrap;align-items:center;gap:0 16px;min-width:0;margin:0 0 0 4px;padding:0;list-style:none}.chips:empty{display:none}.chip{display:flex;align-items:center;gap:6px;min-width:0;max-width:100%;font-size:12px}`,
  `.chip-icon{display:inline-flex;flex:none;align-items:center;justify-content:center;width:16px;color:var(--vscode-descriptionForeground)}.chip-name{min-width:0;max-width:220px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.chip .switch{gap:6px;color:var(--vscode-descriptionForeground)}.chip .remove{margin-left:-4px}`,
  `.spinner{display:inline-block;width:10px;height:10px;border:1.5px solid currentColor;border-right-color:transparent;border-radius:50%;animation:spin 1s linear infinite}`,
  // The action bar and the one problem line above it stay at the foot of the window. A control of the page the keyboard
  // reaches scrolls into view above them, with room for its focus outline: the view keeps --dock at their height. The
  // margin is the controls' own, not the window's scroll padding, so focusing an act in the bar itself scrolls nothing.
  `.page :is(a,button,input,select,textarea,[tabindex]){scroll-margin:8px 0 calc(var(--dock,48px) + 8px)}.dock{position:sticky;bottom:0;z-index:4}.bar-notice{padding:8px 16px;background:var(--vscode-editor-background)}.bar{border-top:0}`,
  // A narrow window: narrower gutters.
  `@media(max-width:600px){.page{padding:28px 20px 40px}.head{margin-bottom:24px}}`,
].join("\n");

const placeholder = `<div class="placeholder"><div class="sk tile"></div><div class="sk sk-line"></div></div>`;
const loadingGroup = (count: number) => `<div class="loading-group"><div class="sk heading-bar"></div><div class="cards">${placeholder.repeat(count)}</div></div>`;
const iconAct = (id: string, label: string, icon: string, extra = "") => `<button type="button" id="${id}" class="icon-button tool" aria-label="${escape(label)}" title="${escape(label)}"${extra}>${icon}</button>`;

/** The New email page. `script` is the view script's address and `nonce` the page's script nonce. */
export function newEmailPage(options: { script: string; nonce: string }): string {
  const body = `<main class="page">
<header class="head"><h1 class="title">${escape(C.title)}</h1><p id="meta" class="meta" hidden></p></header>
<p id="no-organization" class="notice line" hidden>${escape(C.noOrganizationLine)}</p>
<div id="form" class="form">
<div id="organization-row" class="field narrow" hidden><label for="organization">${escape(C.organization)}</label><select id="organization"></select></div>
<div id="layout-group" class="field">
<div id="layouts" class="layouts loading" role="radiogroup" aria-label="${escape(C.layout)}">
<div id="groups" class="layouts"></div>
<div id="placeholders" aria-hidden="true">${PLACEHOLDERS.map(loadingGroup).join("")}</div>
</div></div>
<div class="field narrow"><label for="name">${escape(C.name)}</label><input id="name" autocomplete="off" placeholder="${escape(C.namePlaceholder)}" aria-describedby="name-notice"><p id="name-notice" class="notice" role="alert"></p></div>
<div class="field"><label for="description">${escape(C.describe)} <span class="optional">${escape(C.optional)}</span></label>
<textarea id="description" rows="5" maxlength="8000" placeholder="${escape(C.describePlaceholder)}" aria-describedby="describe-notice"></textarea>
<div class="tools">${iconAct("attach", C.addReferences, ICONS.attach)}${iconAct("dictate", C.dictate, ICONS.mic, ' aria-pressed="false" hidden')}<ul id="references" class="chips" aria-label="${escape(C.references)}"></ul></div>
<p id="dictation-status" class="notice" role="status" aria-live="polite"></p>
<div id="describe-notice" class="notice" role="alert"></div>
</div>
<div class="field"><span id="finished-label" class="label">${escape(C.finishedPicture)} <span class="optional">${escape(C.optionalWord)}</span></span>
<div class="tools"><button type="button" id="attach-reference" class="icon-button tool" aria-label="${escape(C.addFinishedPicture)}" title="${escape(C.addFinishedPicture)}" aria-describedby="finished-label">${ICONS.picture}</button>
<ul id="finished" class="chips" aria-label="${escape(C.finishedPicture)}"></ul></div></div>
</div>
</main>
<div class="dock"><div id="bar-notice" class="notice bar-notice" role="alert" hidden></div>
<footer id="bar" class="bar"><span id="bar-state" class="bar-state" role="status" aria-live="polite" tabindex="-1"></span><div id="bar-acts" class="bar-acts"></div></footer></div>`;
  return pageShell({ title: C.title, nonce: options.nonce, script: options.script, styles: BASE_STYLES + "\n" + STYLES, body });
}
