// The email editor's page: the static markup and styles of the Design view. The
// view script (emailView.ts) fills it from the host's messages; nothing here
// depends on the email, so the host builds it once per panel.
//
// Pure: no vscode and no DOM. Every word comes from EMAIL_COPY.

import { EMAIL_COPY } from "./emailCopy.js";
import { THEME_COLOURS, THEME_TOKENS, type AddableType, type EditableField } from "./emailDocument.js";
import { BASE_STYLES, ICONS, pageShell } from "./pageBase.js";

const C = EMAIL_COPY.editor;

const escape = (value: string) => value.replace(/[&<>"']/g, character => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[character]!));

/** What Add after offers: the leaf blocks, in the order a writer reaches for them. */
const ADDABLE: readonly AddableType[] = ["heading", "text", "highlight", "button", "image", "divider", "spacer"];

/**
 * The order the inspector lists a block's fields in. Every type's fields
 * (EDITABLE_FIELDS) keep their own order within it, so the rows never move.
 */
const FIELD_ORDER: readonly EditableField[] = ["text", "label", "src", "alt", "href", "title", "links", "lines", "html", "height", "color", "background", "align"];


const notice = (id: string) => `<p id="${id}-notice" class="notice" role="alert"></p>`;
/** A control a refusal can be about: its notice describes it. */
const described = (id: string) => `id="${id}" aria-describedby="${id}-notice"`;
const iconAct = (id: string, label: string, icon: string, extra = "") => `<button type="button" id="${id}" class="icon-button" aria-label="${escape(label)}" title="${escape(label)}"${extra}>${icon}</button>`;
const textField = (id: string, label: string, control: string) => `<div class="field"><label for="${id}">${escape(label)}</label>${control}${notice(id)}</div>`;

/** One row of the selected block's fields. Each starts hidden; the view shows the ones its type has. */
function fieldRow(field: EditableField): string {
  const id = `field-${field}`;
  const row = (inner: string) => `<div class="field" data-field="${field}" hidden>${inner}</div>`;
  const labelled = (label: string, control: string) => row(`<label for="${id}">${escape(label)}</label>${control}${notice(id)}`);
  const group = (label: string, inner: string) => row(`<span class="label" id="${id}-label">${escape(label)}</span>${inner}`);
  // "Default" is no colour of the block's own: the layout's. The swatch keeps its place, so the select never moves.
  const colour = (label: string) => labelled(label, `<div class="colour"><select id="${id}"><option value="">${escape(C.colourDefault)}</option>${THEME_TOKENS.map(token => `<option value="${token}">${escape(C.themeColours[token])}</option>`).join("")}</select><span id="${id}-swatch" class="swatch empty" aria-hidden="true"></span></div>`);
  switch (field) {
    case "text": return labelled(C.words, `<textarea ${described(id)} rows="3" spellcheck="true"></textarea>`);
    case "label": return labelled(C.label, `<input ${described(id)} autocomplete="off">`);
    // The block's own title already says Picture: the row's name is for a screen reader alone.
    case "src": return row(`<span class="sr-only" id="${id}-label">${escape(C.picture)}</span><div id="${id}" class="picture" role="group" aria-labelledby="${id}-label"><span id="picture-none" class="muted">${escape(C.noPicture)}</span><button type="button" id="picture-choose" class="text-act">${escape(C.chooseFromFiles)}</button><button type="button" id="picture-upload" class="text-act">${escape(C.upload)}</button><button type="button" id="picture-remove" class="text-act">${escape(C.removePicture)}</button></div>`);
    case "alt": return labelled(C.description, `<input ${described(id)} autocomplete="off">`);
    case "href": return labelled(C.link, `<input ${described(id)} autocomplete="off" spellcheck="false" inputmode="url">`);
    case "title": return labelled(C.title, `<input ${described(id)} autocomplete="off">`);
    case "links": return group(C.links, `<div id="${id}" class="links" role="group" aria-labelledby="${id}-label"></div><button type="button" id="links-add" class="text-act add-link">${escape(C.addLink)}</button>${notice(id)}`);
    case "lines": return labelled(C.lines, `<textarea ${described(id)} rows="3"></textarea>`);
    case "html": return labelled(C.html, `<textarea ${described(id)} class="mono" rows="8" spellcheck="false"></textarea>`);
    case "height": return labelled(C.height, `<input ${described(id)} type="number" min="0" max="2000" step="1" inputmode="numeric">`);
    case "color": return colour(C.colour);
    case "background": return colour(C.background);
    case "align": return group(C.align, `<div id="${id}" class="views align" role="radiogroup" aria-labelledby="${id}-label">${(["left", "center", "right"] as const).map((value, index) => `<button type="button" role="radio" aria-checked="false" data-value="${value}" tabindex="${index === 0 ? 0 : -1}">${escape(C[value])}</button>`).join("")}</div>`);
  }
}

const STYLES = [
  // The frame of the page: a toolbar, the preview beside the inspector, the action bar.
  `html,body{height:100%}body{display:flex;flex-direction:column;overflow:hidden}`,
  `.toolbar{flex:none;display:grid;grid-template-columns:1fr auto 1fr;align-items:center;gap:12px;padding:8px 16px;border-bottom:1px solid var(--vscode-panel-border)}.toolbar .widths{justify-self:end}`,
  `.workspace{flex:1;min-height:0;display:flex}`,
  // The preview: the email at real size, filling its column with the email's own page colour.
  `.stage{position:relative;flex:1;min-width:0;overflow:auto;overscroll-behavior:contain}`,
  `.frames{position:relative;overflow:hidden;margin:0 auto}.frames.away{position:absolute;left:0;top:0;visibility:hidden;pointer-events:none}.stage.mobile .frames{margin:24px auto;box-shadow:0 0 0 1px var(--vscode-panel-border)}`,
  `.frames iframe{display:block;border:0;background:transparent;transform-origin:0 0}.frames iframe:not(.front){position:absolute;left:0;top:0;visibility:hidden;pointer-events:none}`,
  `.skeleton{padding:32px 24px}.plain{max-width:72ch;margin:0 auto;padding:32px 24px;font-family:var(--vscode-editor-font-family,monospace);font-size:var(--vscode-editor-font-size,12px);line-height:1.6;white-space:pre-wrap;overflow-wrap:anywhere}`,
  `.stage.stale .frames,.stage.stale .skeleton,.stage.stale .plain{opacity:.45}.preview-notice{position:sticky;top:0;left:0;z-index:2;padding:10px 16px;background:var(--vscode-editor-background);border-bottom:1px solid var(--vscode-panel-border)}`,
  // The inspector: groups under subheads, no boxes.
  // A control the keyboard reaches scrolls into view with room for its focus outline.
  `.inspector{flex:none;display:flex;flex-direction:column;gap:28px;width:340px;padding:16px 20px 32px;overflow:auto;overscroll-behavior:contain;scroll-padding:8px 0;border-left:1px solid var(--vscode-panel-border)}`,
  `.group{display:flex;flex-direction:column;gap:12px;min-width:0}.field{display:flex;flex-direction:column;gap:4px;min-width:0}.field>label,.field>.label{font-size:12px;color:var(--vscode-descriptionForeground)}.notice:empty{display:none}.muted{color:var(--vscode-descriptionForeground)}.mono{font-family:var(--vscode-editor-font-family,monospace);font-size:var(--vscode-editor-font-size,12px)}`,
  // The outline: one row per block, indented by level, a dot on a block with an issue.
  `.outline{display:flex;flex-direction:column;margin:0 -8px}.outline [role=treeitem]{display:flex;align-items:center;gap:8px;min-height:34px;padding-right:8px;border-radius:4px;cursor:pointer}.outline [role=treeitem]:hover{background:var(--vscode-list-hoverBackground,var(--vscode-toolbar-hoverBackground))}`,
  // The selected row keeps its mark under the pointer: its rule is as specific as the hover's and comes after it. A high
  // contrast theme has no selection background, so the row is outlined there; the outline gives way to the focus ring.
  `.outline [role=treeitem][aria-selected=true]{background:var(--vscode-list-inactiveSelectionBackground)}.outline [role=treeitem][aria-selected=true]:not(:focus-visible){outline:1px solid var(--vscode-contrastActiveBorder,transparent);outline-offset:-1px}.outline [role=treeitem]:focus-visible{outline-offset:-2px}.row-label{min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.dot{flex:none;width:6px;height:6px;margin-left:auto;border-radius:50%;background:var(--vscode-errorForeground)}`,
  `.issue-list{display:flex;flex-direction:column;gap:2px;margin:0;padding:0;list-style:none}.issue-list .text-act{justify-content:flex-start;width:100%;padding:6px 0;text-align:left}.issue-line{display:block;padding:6px 0}`,
  // The selected block: its name and acts, then its fields.
  `.block-head{display:flex;align-items:center;justify-content:space-between;gap:8px}.block-acts{position:relative;display:flex;gap:2px}`,
  `.menu{position:absolute;right:0;top:100%;z-index:5;display:flex;flex-direction:column;min-width:168px;margin-top:4px;padding:4px;background:var(--vscode-editorWidget-background,var(--vscode-editor-background));border:1px solid var(--vscode-widget-border,var(--vscode-panel-border));border-radius:6px;box-shadow:0 8px 24px var(--vscode-widget-shadow,rgba(0,0,0,.16))}.menu [role=menuitem]{justify-content:flex-start;width:100%}.menu [role=menuitem]:focus-visible{outline-offset:-2px}`,
  `.colour{display:flex;align-items:center;gap:8px}.swatch{flex:none;width:12px;height:12px;border-radius:3px;box-shadow:inset 0 0 0 1px color-mix(in srgb,var(--vscode-editor-foreground) 25%,transparent)}.swatch.empty{visibility:hidden}`,
  `.picture{display:flex;flex-wrap:wrap;align-items:center;gap:0 12px;min-height:34px}.picture .text-act{padding:6px 0}.align{align-self:flex-start}.align button{min-width:0}`,
  `.links{display:flex;flex-direction:column;gap:8px}.link-row{display:grid;grid-template-columns:minmax(0,1fr) minmax(0,1.4fr) 34px;gap:8px;align-items:end}.link-row label{display:flex;flex-direction:column;gap:4px;min-width:0;font-size:12px;color:var(--vscode-descriptionForeground)}.add-link{align-self:flex-start;padding:6px 0}`,
  `.custom-line{display:flex;flex-wrap:wrap;align-items:center;gap:4px 12px;margin:0}.custom-line .text-act{padding:6px 0}`,
  // Disclosures: the theme and the sample values.
  `.disclosure>summary{display:list-item}.disclosure[open]>summary{margin-bottom:8px}.disclosure-body{display:flex;flex-direction:column;gap:12px}`,
  `.colours{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:4px 16px}.colours label{display:flex;align-items:center;justify-content:space-between;gap:8px;min-height:34px;font-size:12px;color:var(--vscode-descriptionForeground);cursor:pointer}`,
  `input[type=color]{flex:none;width:44px;height:28px;padding:2px;border:1px solid var(--vscode-input-border,var(--vscode-panel-border));border-radius:6px;background:var(--vscode-input-background);cursor:pointer}input[type=color]:focus-visible{outline:2px solid var(--vscode-focusBorder);outline-offset:2px}`,
  // The action bar's inline layout name, and the one notice line above the bar.
  `.bar-form{display:flex;flex:1;align-items:center;justify-content:flex-end;gap:8px;min-width:0}.bar-form label{font-size:13px;color:var(--vscode-descriptionForeground);white-space:nowrap}.bar-form input{max-width:280px}`,
  `.bar-notice{flex:none;padding:8px 16px;border-top:1px solid var(--vscode-panel-border);background:var(--vscode-editor-background)}`,
  // A narrow window: the inspector becomes a sheet under the preview, above the action bar.
  `@media(max-width:999px){.workspace{flex-direction:column}.inspector{width:auto;height:44vh;gap:24px;padding:12px 16px 24px;border-left:0;border-top:1px solid var(--vscode-panel-border);background:var(--vscode-editorWidget-background,var(--vscode-editor-background))}}`,
  // Below 600 pixels the layout name's label is for screen readers alone: the field keeps its name.
  `@media(max-width:600px){.toolbar{padding:8px}.bar-form label{position:absolute;width:1px;height:1px;overflow:hidden;clip-path:inset(50%);white-space:nowrap}}`,
].join("\n");

/** The Design page of an `*.email.json` file. `script` is the view script's address and `nonce` the page's script nonce. */
export function emailPage(options: { title: string; script: string; nonce: string }): string {
  const body = `<header class="toolbar"><span></span>
<div class="views" role="group" aria-label="${escape(C.emailView)}"><button type="button" id="view-design" aria-pressed="true">${ICONS.design}<span>${escape(C.design)}</span></button><button type="button" id="view-source" aria-pressed="false">${ICONS.source}<span>${escape(C.source)}</span></button></div>
<div class="views widths" role="radiogroup" aria-label="${escape(C.previewWidth)}">${iconAct("width-desktop", C.desktop, ICONS.desktop, ' role="radio" aria-checked="true" tabindex="0"')}${iconAct("width-mobile", C.mobile, ICONS.mobile, ' role="radio" aria-checked="false" tabindex="-1"')}${iconAct("width-plain", C.plain, ICONS.plain, ' role="radio" aria-checked="false" tabindex="-1"')}</div>
</header>
<div class="workspace">
<main id="preview" class="stage">
<div id="preview-notice" class="notice preview-notice" role="alert" hidden></div>
<div id="skeleton" class="skeleton" hidden></div>
<pre id="plain" class="plain" hidden></pre>
<div id="frames" class="frames away"><iframe id="frame-a" class="front" sandbox="allow-same-origin" title="${escape(C.emailPreview)}"></iframe><iframe id="frame-b" sandbox="allow-same-origin" title="${escape(C.emailPreview)}"></iframe></div>
</main>
<aside id="inspector" class="inspector">
<section id="email-group" class="group" hidden><h2 class="subhead">${escape(C.email)}</h2>
${textField("subject", C.subject, `<input ${described("subject")} autocomplete="off">`)}
${textField("preheader", C.preheader, `<input ${described("preheader")} autocomplete="off">`)}
<div id="organization-row" class="field" hidden><label for="organization">${escape(C.organization)}</label><select id="organization"></select></div>
</section>
<section id="issues" class="group" hidden><h2 class="subhead">${escape(C.issues)}</h2><ul id="issue-list" class="issue-list"></ul></section>
<section id="blocks-group" class="group" hidden><h2 class="subhead" id="blocks-title">${escape(C.blocks)}</h2><div id="outline" class="outline" role="tree" aria-labelledby="blocks-title"></div></section>
<section id="block" class="group" hidden>
<div class="block-head"><h2 id="block-title" class="subhead"></h2><div class="block-acts">${iconAct("move-up", C.moveUp, ICONS.up)}${iconAct("move-down", C.moveDown, ICONS.down)}${iconAct("add-after", C.addAfter, ICONS.plus, ' aria-haspopup="menu" aria-expanded="false" aria-controls="add-menu"')}${iconAct("remove-block", C.remove, ICONS.trash)}
<div id="add-menu" class="menu" role="menu" aria-label="${escape(C.addAfter)}" hidden>${ADDABLE.map(type => `<button type="button" role="menuitem" tabindex="-1" data-type="${type}">${escape(C.blockNames[type])}</button>`).join("")}</div></div></div>
${FIELD_ORDER.map(fieldRow).join("\n")}
<div class="field" data-field="brand" hidden><label class="switch"><input type="checkbox" role="switch" id="field-brand"><span>${escape(C.brand)}</span></label></div>
</section>
<div id="custom-note" class="group" hidden><p class="custom-line"><span id="custom-words">${escape(C.htmlOnly)}</span><button type="button" id="edit-html" class="text-act">${escape(C.editHtml)}</button></p></div>
<details id="theme" class="disclosure" hidden><summary class="subhead">${escape(C.theme)}</summary><div class="disclosure-body">
<div class="colours">${THEME_COLOURS.map(key => `<label><span>${escape(C.themeColours[key])}</span><input type="color" id="theme-${key}"></label>`).join("")}</div>
${textField("theme-headingFont", C.headingFont, `<input ${described("theme-headingFont")} autocomplete="off" spellcheck="false">`)}
${textField("theme-bodyFont", C.bodyFont, `<input ${described("theme-bodyFont")} autocomplete="off" spellcheck="false">`)}
</div></details>
<details id="samples" class="disclosure" hidden><summary class="subhead">${escape(C.previewWith)}</summary><div id="sample-fields" class="disclosure-body"></div></details>
</aside>
</div>
<div id="bar-notice" class="notice bar-notice" role="alert" hidden></div>
<footer id="bar" class="bar"><span id="bar-state" class="bar-state" role="status" aria-live="polite" tabindex="-1"></span><div id="bar-acts" class="bar-acts"></div>
<div id="bar-form" class="bar-form" hidden><label for="layout-name">${escape(C.layoutName)}</label><input id="layout-name" autocomplete="off"><button type="button" id="layout-cancel" class="text-act">${escape(C.cancel)}</button><button type="button" id="layout-save" class="primary">${escape(C.saveLayoutConfirm)}</button></div></footer>`;
  return pageShell({ title: options.title, nonce: options.nonce, script: options.script, styles: BASE_STYLES + "\n" + STYLES, body });
}
