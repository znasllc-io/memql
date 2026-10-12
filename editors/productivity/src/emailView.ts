// The email editor's Design view, running inside the webview. It draws what the
// host sends (the document, the engine's render of it, the connection and the
// action bar) and sends back what the person does: edits as EmailEdit
// operations, acts, and the page's own state (the selection, the width, the
// open disclosures and the sample values). The markup is emailPage.ts's.
//
// Host messages count only from this webview's own origin. The preview frames
// are sandboxed without scripts; the page reaches into them (same origin) to
// read clicks and to mark the selected block, nothing else.

import { EMAIL_COPY } from "./emailCopy.js";
import { EDITABLE_FIELDS, THEME_COLOURS, THEME_TOKENS, blockLabel, canMove, editEmail, isLayoutEmail, sampleKeys, validLink, walkBlocks, type AddableType, type EditableField, type EmailBlock, type EmailDocument, type EmailEdit } from "./emailDocument.js";
import { markdownLinkProblem } from "./emailMarkdown.js";
import { PREVIEW_CSP, previewHTML, skeletonHTML } from "./emailPreview.js";
import { ICONS } from "./pageBase.js";
import type { Problem } from "./problems.js";
import { renderProblem } from "./problemView.js";
import { UserInputError } from "./userError.js";

declare function acquireVsCodeApi(): { postMessage(message: unknown): void; getState(): unknown; setState(state: unknown): void };

type Width = "desktop" | "mobile" | "plain";
type BarAct = "connect" | "openSource" | "saveToMemQL" | "saveLayout" | "publish";
interface ViewState { width: Width; selected?: string; themeOpen?: boolean; samplesOpen?: boolean; samples?: Record<string, string> }
interface Issue { code: string; block?: string; message: string }
interface Placed { block: EmailBlock; brand: boolean }

const C = EMAIL_COPY.editor;
/** Typing is sent once the person pauses this long, or leaves the field. */
const PAUSE = 250;
/** The states in which the host is working: the bar shows its spinner. */
const BUSY: readonly string[] = [C.publishing, C.savingLayout, C.savingCopy];
/** Acts drawn as the bar's button; the others are text acts. */
const BUTTON_ACTS: readonly BarAct[] = ["connect", "openSource", "saveToMemQL", "publish"];
const ACT_WORDS: Readonly<Record<BarAct, string>> = { connect: C.connect, openSource: C.openSource, saveToMemQL: C.saveToMemQL, saveLayout: C.saveLayout, publish: C.publish };
const HEX = /^#[0-9a-fA-F]{6}$/;
/** The most links a social or footer block takes. */
const MAX_LINKS = 8;
/** The desktop frame is never narrower than the email and its margins; the mobile one is a phone's width. */
const DESKTOP = 640, MOBILE = 375;

const api = acquireVsCodeApi();
const post = (message: unknown) => api.postMessage(message);
const byId = <T extends HTMLElement = HTMLElement>(id: string) => document.getElementById(id) as T;
function textElement(tag: string, text: string, className = ""): HTMLElement {
  const element = document.createElement(tag);
  element.textContent = text;
  if (className) element.className = className;
  return element;
}
const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);
const str = (value: unknown) => typeof value === "string" ? value : "";
const same = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);
const bytes = (text: string) => new TextEncoder().encode(text).length;
const characters = (text: string) => { let count = 0; for (const _ of text) count++; return count; };
/** Whether a person can reach the element: on the page and not inside anything hidden. */
const reachable = (element: Element) => element.isConnected && !element.closest("[hidden]");
/**
 * Whether the person is working in `box` now. The page keeps its active element
 * while another window has the keyboard (the Source editor beside it in Split,
 * say), so the window's focus is asked as well.
 */
const holds = (box: Element) => document.hasFocus() && box.contains(document.activeElement);

// -- The page -----------------------------------------------------------------

const preview = byId("preview"), previewNotice = byId("preview-notice"), skeleton = byId("skeleton"), plain = byId("plain"), framesBox = byId("frames");
const frames = [byId<HTMLIFrameElement>("frame-a"), byId<HTMLIFrameElement>("frame-b")];
const widthButtons: Readonly<Record<Width, HTMLElement>> = { desktop: byId("width-desktop"), mobile: byId("width-mobile"), plain: byId("width-plain") };
const emailGroup = byId("email-group"), subject = byId<HTMLInputElement>("subject"), preheader = byId<HTMLInputElement>("preheader");
const organizationRow = byId("organization-row"), organization = byId<HTMLSelectElement>("organization");
const issuesGroup = byId("issues"), issueList = byId("issue-list");
const blocksGroup = byId("blocks-group"), outline = byId("outline");
const blockGroup = byId("block"), blockTitle = byId("block-title");
const moveUp = byId("move-up"), moveDown = byId("move-down"), addAfter = byId("add-after"), addMenu = byId("add-menu"), removeBlock = byId("remove-block");
const menuItems = Array.from(addMenu.querySelectorAll<HTMLButtonElement>('[role="menuitem"]'));
const fieldRows = Array.from(blockGroup.querySelectorAll<HTMLElement>("[data-field]"));
const customNote = byId("custom-note"), customWords = byId("custom-words");
const theme = byId<HTMLDetailsElement>("theme"), samplesBox = byId<HTMLDetailsElement>("samples"), sampleFields = byId("sample-fields");
const barState = byId("bar-state"), barActs = byId("bar-acts"), barForm = byId("bar-form"), layoutName = byId<HTMLInputElement>("layout-name"), barNotice = byId("bar-notice");

// -- What the page knows -------------------------------------------------------

/** The last email that could be read; while the text can't be read, `invalid` says why. */
let email: EmailDocument | undefined;
let invalid: string | undefined;
let version = -1;
/** Unknown until the host says. */
let connected: boolean | undefined;
/** Every block by id (the first, should an id repeat), and all of them in outline order. */
let placed = new Map<string, Placed>();
let order: string[] = [];
let selected: string | undefined;
/** A selection asked for before its block arrived: from a restore (not posted back) or the host after an add (focus its first field). */
let pendingSelect: { id: string; post: boolean; focusField: boolean } | undefined;
let removing: string | undefined;
let awaitingAdd = false;
let width: Width = "desktop";
let themeOpen = false, samplesOpen = false;
let samples: Record<string, string> = {};
let issues: Issue[] = [];
let lastSeq = Number.NEGATIVE_INFINITY;
let renderText: string | undefined;
/** The frame on screen, whether a render is showing at all, and why it is dimmed. */
let front = 0;
let frameShown = false;
let stale: "failed" | "invalid" | undefined;
/** The render each frame is loading, and the last one that failed: a render older than a failure does not clear it. */
const loading = new Map<HTMLIFrameElement, number>();
let failedSeq = Number.NEGATIVE_INFINITY;
/** A render can come only for a connected email whose organization is known. */
let organizationKnown = false;
let pageColour = "";
let skeletonFor: EmailDocument | undefined;
let bar: { state: string; acts: BarAct[]; layoutName?: string } = { state: "", acts: [] };
/** An act was sent; the next one waits for the host's answer (a bar or a problem). */
let actPending = false;

const layout = () => !!email && invalid === undefined && isLayoutEmail(email);
const custom = () => !!email && invalid === undefined && !isLayoutEmail(email);
/** An email written in HTML with no HTML at all: plain text is all there is to show. */
const htmlless = () => custom() && str((email as { htmlBody?: unknown }).htmlBody).trim() === "";
const effectiveWidth = (): Width => htmlless() ? "plain" : width;
const selectedPlace = () => selected === undefined ? undefined : placed.get(selected);
const selectedBlock = () => selectedPlace()?.block;

// -- Fields --------------------------------------------------------------------
//
// A field sends its value once the person pauses or leaves it, and only when
// it differs from what the document (or the edit already sent) says. Before
// sending, it refuses what the engine's parser would refuse, in its own notice:
// quietly while typing goes on, out loud on leaving, and it clears itself the
// moment the value is right. A document update never overwrites a field being
// typed in, typing waiting for its pause, or a sent edit the host has not
// answered yet. Typing the field refused stays while the person is in the
// page; a newer document that arrives while they work elsewhere (Source, in
// Split) replaces it.

interface Field {
  /** The control, or the group of controls, the value lives in. */
  readonly control: HTMLElement;
  readonly notice?: HTMLElement;
  /** Typed fields keep what is typed while they have focus; choices follow the document. */
  readonly typed: boolean;
  /** What the field edits now ("subject", "block:<id>:text"), or nothing. */
  key(): string | undefined;
  read(): unknown;
  /** The document's value, in the form read() gives. */
  current(): unknown;
  show(value: unknown): void;
  edit(value: unknown): EmailEdit;
  /** A refusal the model leaves to the engine's parser. */
  check?(value: unknown): string | undefined;
  /** The controls a refusal can be about (the control itself unless said), and the one it is about now. */
  controls?(): HTMLElement[];
  offender?(): HTMLElement | undefined;
}

/** Shows a field's refusal in its notice, or clears it, and marks the control it is about for a screen reader. */
function tell(field: Field, text: string): void {
  const notice = field.notice;
  if (!notice) return;
  if (notice.textContent !== text) notice.textContent = text;
  const about = text ? field.offender?.() ?? field.control : undefined;
  for (const control of field.controls?.() ?? [field.control]) {
    if (control === about) control.setAttribute("aria-invalid", "true");
    else control.removeAttribute("aria-invalid");
  }
}

const timers = new Map<Field, number>();
/** Typing not yet sent, by key: the document version it was typed against. */
const dirty = new Map<string, number>();
const sent = new Map<string, { value: string; version: number }>();

/** The model's own refusal of an edit, as the sentence it gives. */
function refusal(edit: EmailEdit): string | undefined {
  if (!email) return undefined;
  try {
    editEmail(email, edit);
    return undefined;
  } catch (error) {
    if (error instanceof UserInputError) return error.message;
    throw error;
  }
}

function changed(field: Field): void {
  const key = field.key();
  if (key === undefined) return;
  dirty.set(key, version);
  window.clearTimeout(timers.get(field));
  timers.set(field, window.setTimeout(() => commit(field, false), PAUSE));
}

function commit(field: Field, leaving: boolean): void {
  window.clearTimeout(timers.get(field));
  timers.delete(field);
  const key = field.key();
  if (key === undefined || !dirty.has(key) || !email || invalid !== undefined) return;
  const value = field.read();
  const before = sent.get(key);
  if (same(value, before ? JSON.parse(before.value) : field.current())) {
    dirty.delete(key);
    tell(field, "");
    return;
  }
  const edit = field.edit(value);
  const problem = field.check?.(value) ?? refusal(edit);
  if (problem !== undefined) {
    if (leaving || field.notice?.textContent) tell(field, problem);
    return;
  }
  tell(field, "");
  dirty.delete(key);
  sent.set(key, { value: JSON.stringify(value), version });
  post({ type: "edit", op: edit });
}

/** Puts the document's value in the field, unless the person's own value has to stay. */
function refresh(field: Field): void {
  const key = field.key();
  if (field.typed && holds(field.control)) return;
  if (key !== undefined && dirty.has(key)) {
    // Only a draft whose refusal is showing gives way; a value still waiting for its pause (a colour being picked) is the person's.
    if (holds(document.body) || version <= dirty.get(key)! || !field.notice?.textContent) return;
    dirty.delete(key);
    tell(field, "");
  }
  if (key !== undefined && sent.has(key)) return;
  field.show(field.current());
}

/** Typing, leaving and Ctrl or Cmd+Enter on a typed control. */
function listen(field: Field, control: HTMLElement = field.control): void {
  control.addEventListener("input", () => changed(field));
  control.addEventListener("blur", () => commit(field, true));
  control.addEventListener("keydown", event => {
    if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) { event.preventDefault(); commit(field, true); }
  });
}

/** A choice (a select, a radio, a switch) sends at once. */
function chose(field: Field): void {
  const key = field.key();
  if (key === undefined) return;
  dirty.set(key, version);
  commit(field, true);
}

function textField(control: HTMLInputElement | HTMLTextAreaElement, notice: HTMLElement, key: () => string | undefined, current: () => string, edit: (value: string) => EmailEdit, check?: (value: string) => string | undefined): Field {
  const field: Field = {
    control, notice, typed: true, key, current,
    read: () => control.value,
    show: value => { control.value = str(value); },
    edit: value => edit(value as string),
    check: check && (value => check(value as string)),
  };
  listen(field);
  return field;
}

const blockKey = (name: string) => () => selected === undefined ? undefined : `block:${selected}:${name}`;
const fieldEdit = (field: EditableField) => (value: unknown): EmailEdit => ({ kind: "field", block: selected!, field, value: value as never });
const blockText = (name: "text" | "label" | "alt" | "href" | "title" | "html", check?: (value: string) => string | undefined) =>
  textField(byId<HTMLInputElement>(`field-${name}`), byId(`field-${name}-notice`), blockKey(name), () => str(selectedBlock()?.[name]), fieldEdit(name), check);

const subjectField = textField(subject, byId("subject-notice"), () => "subject", () => str(email?.subject), value => ({ kind: "subject", value }));
// A preheader set to null is no preheader.
const preheaderField = textField(preheader, byId("preheader-notice"), () => "preheader", () => str(email?.preheader), value => ({ kind: "preheader", value }));

const wordsField = blockText("text", value => {
  const problem = selectedBlock()?.type === "text" ? markdownLinkProblem(value) : undefined;
  return problem === "label" ? C.wordsLinkLabel : problem === "address" ? C.wordsLinkAddress : undefined;
});
// The engine counts a label in bytes; the person reads only that it is missing or too long.
const labelField = blockText("label", value => { const size = bytes(value); return size < 1 ? C.buttonLabelEmpty : size > 200 ? C.buttonLabelInvalid : undefined; });

const heightControl = byId<HTMLInputElement>("field-height");
const heightField: Field = {
  control: heightControl, notice: byId("field-height-notice"), typed: true, key: blockKey("height"),
  read: () => heightControl.value.trim() === "" ? "" : Number(heightControl.value),
  current: () => { const height = selectedBlock()?.height; return typeof height === "number" ? height : ""; },
  show: value => { heightControl.value = typeof value === "number" ? String(value) : ""; },
  edit: fieldEdit("height"),
};
listen(heightField);

const linesControl = byId<HTMLTextAreaElement>("field-lines");
const linesField: Field = {
  control: linesControl, notice: byId("field-lines-notice"), typed: true, key: blockKey("lines"),
  // An empty line at the end is not a line of the footer (the engine would count it toward the twelve).
  read: () => { const lines = linesControl.value.split("\n"); while (lines.length && lines[lines.length - 1] === "") lines.pop(); return lines; },
  current: () => { const lines = selectedBlock()?.lines; return Array.isArray(lines) ? lines : []; },
  show: value => { linesControl.value = (Array.isArray(value) ? value : []).map(str).join("\n"); },
  edit: fieldEdit("lines"),
  check: value => {
    const lines = value as string[];
    if (lines.length > 12) return C.tooManyLines;
    return lines.some(line => characters(line) > 300) ? C.lineTooLong : undefined;
  },
};
listen(linesField);

// Links: a row per link (its network or label, and its address), Add link, and a remove act per row.
const linksBox = byId("field-links"), linksAdd = byId("links-add");
/** The block's links when the rows were drawn: a row keeps the keys it does not show (a social icon, say). */
let linkBase: Record<string, unknown>[] = [];
let linkSocial = false;
const linksField: Field = {
  control: linksBox, notice: byId("field-links-notice"), typed: true, key: blockKey("links"),
  read: () => Array.from(linksBox.querySelectorAll<HTMLElement>(".link-row"), row => {
    const index = Number(row.dataset.index);
    const link: Record<string, unknown> = { ...(index >= 0 ? linkBase[index] : {}) };
    link[linkSocial ? "network" : "label"] = row.querySelector<HTMLInputElement>('[data-part="name"]')!.value;
    const href = row.querySelector<HTMLInputElement>('[data-part="href"]')!.value;
    if (href) link.href = href; else delete link.href;
    return link;
  }),
  current: () => { const links = selectedBlock()?.links; return Array.isArray(links) ? links : []; },
  show: value => drawLinks(value),
  edit: fieldEdit("links"),
  check: value => {
    for (const link of value as Record<string, unknown>[]) {
      const problem = linkNameProblem(linkSocial ? str(link.network) : str(link.label));
      if (problem) return problem;
    }
    return undefined;
  },
  controls: () => Array.from(linksBox.querySelectorAll<HTMLElement>("input")),
  // The first name that is wrong, else the first address the link rule refuses.
  offender: () => {
    const rows = Array.from(linksBox.querySelectorAll<HTMLElement>(".link-row"));
    const part = (row: HTMLElement, name: string) => row.querySelector<HTMLInputElement>(`[data-part="${name}"]`)!;
    const named = rows.find(row => linkNameProblem(part(row, "name").value));
    if (named) return part(named, "name");
    const addressed = rows.find(row => !validLink(part(row, "href").value));
    return addressed && part(addressed, "href");
  },
};

/** A social network is 1 to 40 bytes and a footer link's label 1 to 80 characters; the person reads only that it is missing or too long. */
function linkNameProblem(name: string): string | undefined {
  if (linkSocial) { const size = bytes(name); return size < 1 ? C.networkEmpty : size > 40 ? C.networkInvalid : undefined; }
  const count = characters(name);
  return count < 1 || count > 80 ? C.linkLabelInvalid : undefined;
}

function drawLinks(value: unknown): void {
  linkSocial = selectedBlock()?.type === "social";
  linkBase = (Array.isArray(value) ? value : []).map(link => isRecord(link) ? link : {});
  linksBox.replaceChildren(...linkBase.map((link, index) => linkRow(link, index)));
  linkActs();
}

function linkRow(link: Record<string, unknown>, index: number): HTMLElement {
  const row = textElement("div", "", "link-row");
  row.dataset.index = String(index);
  const part = (name: "name" | "href", label: string, value: string) => {
    const wrap = document.createElement("label");
    const input = document.createElement("input");
    input.dataset.part = name;
    input.value = value;
    input.autocomplete = "off";
    input.setAttribute("aria-describedby", "field-links-notice");
    if (name === "href") { input.spellcheck = false; input.inputMode = "url"; }
    listen(linksField, input);
    wrap.append(textElement("span", label), input);
    return wrap;
  };
  const remove = textElement("button", "", "icon-button link-remove") as HTMLButtonElement;
  remove.type = "button";
  remove.setAttribute("aria-label", C.removeLink);
  remove.title = C.removeLink;
  remove.innerHTML = ICONS.trash; // a constant drawing, no text from the email
  remove.addEventListener("click", () => {
    const neighbour = (row.nextElementSibling ?? row.previousElementSibling) as HTMLElement | null;
    row.remove();
    linkActs();
    chose(linksField);
    (neighbour?.querySelector("input") ?? linksAdd).focus();
  });
  row.append(part("name", linkSocial ? C.network : C.label, str(linkSocial ? link.network : link.label)), part("href", C.link, str(link.href)), remove);
  return row;
}

/** Add link while there is room; a social block keeps at least one link. */
function linkActs(): void {
  const rows = linksBox.querySelectorAll(".link-row").length;
  linksAdd.hidden = rows >= MAX_LINKS;
  for (const remove of linksBox.querySelectorAll<HTMLElement>(".link-remove")) remove.hidden = linkSocial && rows <= 1;
}

linksAdd.addEventListener("click", () => {
  const key = linksField.key();
  if (key === undefined) return;
  dirty.set(key, version);
  const row = linkRow({}, -1);
  linksBox.append(row);
  linkActs();
  row.querySelector("input")!.focus();
});

// Colours: one of the theme's tokens, with a swatch of the theme's value.
function colourField(name: "color" | "background"): Field {
  const control = byId<HTMLSelectElement>(`field-${name}`);
  const field: Field = {
    control, typed: false, key: blockKey(name),
    read: () => control.value,
    current: () => str(selectedBlock()?.[name]),
    show: value => { control.value = (THEME_TOKENS as readonly unknown[]).includes(value) ? value as string : ""; paintSwatch(name); },
    edit: fieldEdit(name),
  };
  control.addEventListener("change", () => { paintSwatch(name); chose(field); });
  return field;
}

function paintSwatch(name: "color" | "background"): void {
  const token = byId<HTMLSelectElement>(`field-${name}`).value, swatch = byId(`field-${name}-swatch`);
  const value = token === "white" ? "#ffffff" : email && isLayoutEmail(email) && (THEME_COLOURS as readonly string[]).includes(token) ? str(email.theme[token as typeof THEME_COLOURS[number]]) : "";
  swatch.classList.toggle("empty", !HEX.test(value));
  swatch.style.backgroundColor = HEX.test(value) ? value : "";
}

const textColourField = colourField("color"), backgroundField = colourField("background");

// Alignment: three radios; a block without one shows none chosen (the layout's own).
const alignGroup = byId("field-align");
const alignButtons = Array.from(alignGroup.querySelectorAll<HTMLElement>('[role="radio"]'));
let alignValue = "";
const alignField: Field = {
  control: alignGroup, typed: false, key: blockKey("align"),
  read: () => alignValue,
  current: () => str(selectedBlock()?.align),
  show: value => { alignValue = alignButtons.some(button => button.dataset.value === value) ? value as string : ""; paintAlign(); },
  edit: fieldEdit("align"),
};
function paintAlign(): void {
  const chosen = alignButtons.find(button => button.dataset.value === alignValue);
  for (const button of alignButtons) {
    button.setAttribute("aria-checked", String(button === chosen));
    button.tabIndex = button === (chosen ?? alignButtons[0]) ? 0 : -1;
  }
}
function chooseAlign(button: HTMLElement): void {
  alignValue = button.dataset.value ?? "";
  paintAlign();
  chose(alignField);
}
alignButtons.forEach((button, index) => {
  button.addEventListener("click", () => chooseAlign(button));
  button.addEventListener("keydown", event => {
    const step = event.key === "ArrowRight" || event.key === "ArrowDown" ? 1 : event.key === "ArrowLeft" || event.key === "ArrowUp" ? -1 : 0;
    if (!step) return;
    event.preventDefault();
    const next = alignButtons[(index + step + alignButtons.length) % alignButtons.length]!;
    next.focus();
    chooseAlign(next);
  });
});

// "Same in every email": the block's role, as it comes out after its containers'.
const brand = byId<HTMLInputElement>("field-brand");
const brandField: Field = {
  control: brand, typed: false, key: blockKey("role"),
  read: () => brand.checked,
  current: () => selectedPlace()?.brand ?? false,
  show: value => { brand.checked = value === true; },
  edit: value => ({ kind: "role", block: selected!, brand: value === true }),
};
brand.addEventListener("change", () => chose(brandField));

const blockFields: readonly Field[] = [wordsField, labelField, blockText("alt"), blockText("href"), blockText("title"), blockText("html"), heightField, linesField, linksField, textColourField, backgroundField, alignField, brandField];

// The theme: eight colours and two fonts.
const themeFields: readonly Field[] = [
  ...THEME_COLOURS.map(key => {
    const control = byId<HTMLInputElement>(`theme-${key}`);
    const field: Field = {
      control, typed: false, key: () => `theme:${key}`,
      read: () => control.value.toLowerCase(),
      current: () => email && isLayoutEmail(email) ? str(email.theme[key]).toLowerCase() : "",
      // A colour input holds only #rrggbb; anything else stays for Source to show.
      show: value => { if (HEX.test(str(value))) control.value = str(value); },
      edit: value => ({ kind: "theme", key, value: value as string }),
    };
    control.addEventListener("input", () => changed(field));
    // The picker's commit sends at once, whether or not it moved through input events first.
    control.addEventListener("change", () => chose(field));
    return field;
  }),
  ...(["headingFont", "bodyFont"] as const).map(key => textField(byId<HTMLInputElement>(`theme-${key}`), byId(`theme-${key}-notice`), () => `theme:${key}`,
    () => email && isLayoutEmail(email) ? str(email.theme[key]) : "", value => ({ kind: "theme", key, value }))),
];

// -- The preview ---------------------------------------------------------------

const POLICY = `<meta http-equiv="Content-Security-Policy" content="${PREVIEW_CSP}">`;

/** The editor's focus colour as a literal (the frame has no theme variables); only a plain colour value gets in. */
function focusColour(): string {
  const value = getComputedStyle(document.documentElement).getPropertyValue("--vscode-focusBorder").trim();
  return /^[#a-zA-Z0-9(),.%\s-]{1,64}$/.test(value) ? value : "Highlight";
}

/**
 * The frame's own style: a pointer over blocks, a dashed outline on the innermost one hovered, and a ring on the
 * selected one, which the hover leaves alone. The ring is the focus colour with a white line inside it, so it shows on
 * any email: white, tinted, a brand colour near the focus colour, or dark. A block too thin to hold it is ringed around
 * its edge (markFrame).
 */
function frameStyle(): string {
  const colour = focusColour();
  return `<style>[data-block]{cursor:pointer}[data-block]:not([data-selected]):hover:not(:has([data-block]:hover)){outline:1px dashed ${colour}!important;outline-offset:-1px}[data-block][data-selected]{outline:2px solid ${colour}!important;outline-offset:-2px;box-shadow:inset 0 0 0 4px #fff!important}[data-block][data-selected=around]{outline-offset:2px;box-shadow:none!important}</style>`;
}

/** Loads a render into the frame behind the one on screen; they swap once it has loaded. */
function loadRender(html: string, seq: number): void {
  const frame = frames[1 - front]!;
  loading.set(frame, seq);
  frame.setAttribute("data-pending", "1");
  frame.setAttribute("srcdoc", previewHTML(html, { blocks: true }).replace(POLICY, () => POLICY + frameStyle()));
}

function frameLoaded(frame: HTMLIFrameElement): void {
  if (frame.getAttribute("data-pending") !== "1") return;
  const doc = frame.contentDocument;
  if (!doc) return;
  frame.removeAttribute("data-pending");
  doc.addEventListener("click", frameClick, true);
  doc.addEventListener("auxclick", event => event.preventDefault(), true);
  frames[front]!.classList.remove("front");
  frame.classList.add("front");
  front = frames.indexOf(frame);
  frameShown = true;
  // This render is the last good one; a failure since stays, dimming it, and so does an email that can't be read.
  if (stale === "failed" && (loading.get(frame) ?? 0) > failedSeq) {
    stale = undefined;
    previewNotice.hidden = true;
    previewNotice.replaceChildren();
  }
  const colour = doc.defaultView?.getComputedStyle(doc.body).backgroundColor ?? "";
  pageColour = colour && colour !== "transparent" && colour !== "rgba(0, 0, 0, 0)" ? colour : "";
  markFrame();
  drawStage();
}
for (const frame of frames) frame.addEventListener("load", () => frameLoaded(frame));

/** A click in the email never follows anything; it selects the block it landed in. */
function frameClick(event: MouseEvent): void {
  event.preventDefault();
  // A dimmed preview may show blocks whose ids now name others (an added block takes the smallest free id).
  if (stale !== undefined) return;
  const target = event.target as { closest?: (selector: string) => Element | null } | null;
  const id = (typeof target?.closest === "function" ? target.closest("[data-block]") : null)?.getAttribute("data-block");
  if (id && placed.has(id)) select(id, { fields: true });
}

/** The marked blocks of the frame on screen. Ids are compared, never written into a selector. */
function frameBlocks(): Element[] {
  return Array.from(frames[front]!.contentDocument?.querySelectorAll("[data-block]") ?? []);
}

/** A block shorter than this would have the ring lie across what it holds (a divider's line, say): it is ringed around instead. */
const THIN = 24;

function markFrame(): void {
  for (const element of frameBlocks()) {
    if (element.getAttribute("data-block") === selected) element.setAttribute("data-selected", element.getBoundingClientRect().height < THIN ? "around" : "");
    else element.removeAttribute("data-selected");
  }
}

/** Scrolls the preview so the selected block is in view. */
function revealInPreview(): void {
  if (framesBox.classList.contains("away")) return;
  const target = frameBlocks().find(element => element.getAttribute("data-block") === selected);
  if (!target) return;
  const box = target.getBoundingClientRect(), scale = frameScale();
  const top = framesBox.offsetTop + box.top * scale, bottom = top + box.height * scale;
  if (top < preview.scrollTop || bottom > preview.scrollTop + preview.clientHeight) preview.scrollTop = Math.max(0, top - 24);
}

function frameScale(): number {
  const column = preview.clientWidth, mobile = effectiveWidth() === "mobile";
  const room = mobile ? column - 48 : column;
  return room > 0 ? Math.min(1, room / (mobile ? MOBILE : DESKTOP)) : 1;
}

/** Sizes the frames to the column: the desktop frame fills it (at least 640 pixels, scaled down to fit), the mobile one is 375 pixels. */
function layoutFrames(): void {
  const mobile = effectiveWidth() === "mobile", scale = frameScale();
  const frameWidth = mobile ? MOBILE : Math.max(DESKTOP, preview.clientWidth);
  for (const frame of frames) {
    frame.style.width = `${frameWidth}px`;
    frame.style.transform = scale < 1 ? `scale(${scale})` : "";
  }
  // A frame is as tall as its email: measured at no height, so the old height cannot hold it open.
  const shown = frames[front]!;
  shown.style.height = "0px";
  const height = shown.contentDocument?.documentElement.scrollHeight ?? 0;
  shown.style.height = `${height}px`;
  framesBox.style.width = `${Math.round(frameWidth * scale)}px`;
  framesBox.style.height = `${Math.ceil(height * scale)}px`;
}
if (typeof ResizeObserver !== "undefined") new ResizeObserver(() => { if (!framesBox.classList.contains("away")) layoutFrames(); }).observe(preview);

/** What the preview area shows: the render, the skeleton, or plain text. */
function drawStage(): void {
  const effective = effectiveWidth(), offline = connected === false, textBody = custom() ? str((email as { textBody?: unknown }).textBody) : undefined;
  for (const name of ["desktop", "mobile"] as const) widthButtons[name].hidden = htmlless();
  for (const [name, button] of Object.entries(widthButtons) as [Width, HTMLElement][]) {
    button.setAttribute("aria-checked", String(name === effective));
    button.tabIndex = name === effective ? 0 : -1;
  }
  preview.classList.toggle("mobile", effective === "mobile");
  preview.classList.toggle("stale", stale !== undefined);
  let view: "frames" | "skeleton" | "plain" | "none" = "none", text: string | undefined;
  if (email) {
    const live = frameShown && !offline;
    // Plain text is the render's; a custom email without one shows the text it carries.
    if (effective === "plain") text = !offline && renderText !== undefined ? renderText : textBody;
    else if (!live) text = textBody;
    if (text !== undefined) view = "plain";
    else if (effective !== "plain" && live) view = "frames";
    else if (isLayoutEmail(email)) view = "skeleton";
  }
  if (view === "skeleton" && email && isLayoutEmail(email) && skeletonFor !== email) {
    skeleton.innerHTML = skeletonHTML(email.blocks); // numbers only: no text from the email enters it
    skeletonFor = email;
  }
  skeleton.hidden = view !== "skeleton";
  // The sweep says a render is on its way: only while connected, with nothing wrong.
  skeleton.classList.toggle("loading", view === "skeleton" && connected === true && organizationKnown && stale === undefined);
  plain.hidden = view !== "plain";
  if (view === "plain" && plain.textContent !== text) plain.textContent = text ?? "";
  framesBox.classList.toggle("away", view !== "frames");
  preview.style.backgroundColor = view === "frames" && effective === "desktop" ? pageColour : "";
  if (view === "frames") layoutFrames();
}

function chooseWidth(next: Width): void {
  if (next === width && next === effectiveWidth()) return;
  width = next;
  drawStage();
  postViewState();
}
const widthOrder = (["desktop", "mobile", "plain"] as const);
for (const name of widthOrder) {
  const button = widthButtons[name];
  button.addEventListener("click", () => chooseWidth(name));
  button.addEventListener("keydown", event => {
    const step = event.key === "ArrowRight" || event.key === "ArrowDown" ? 1 : event.key === "ArrowLeft" || event.key === "ArrowUp" ? -1 : 0;
    if (!step) return;
    event.preventDefault();
    const offered = widthOrder.filter(item => !widthButtons[item].hidden);
    const next = offered[(offered.indexOf(name) + step + offered.length) % offered.length]!;
    widthButtons[next].focus();
    chooseWidth(next);
  });
}

// -- The outline and the selected block ----------------------------------------

const rowOf = (target: EventTarget | null) => target instanceof Element ? target.closest<HTMLElement>('[role="treeitem"]') : null;
const rows = () => Array.from(outline.querySelectorAll<HTMLElement>('[role="treeitem"]'));
const rowFor = (id: string | undefined) => rows().find(row => row.dataset.block === id);

function drawOutline(): void {
  const active = document.activeElement;
  const focusId = holds(outline) && active instanceof HTMLElement ? active.dataset.block : undefined;
  const list: HTMLElement[] = [];
  if (layout()) walkBlocks((email as { blocks: EmailBlock[] }).blocks, (block, depth) => {
    const row = textElement("div", "");
    row.setAttribute("role", "treeitem");
    row.setAttribute("aria-level", String(depth + 1));
    row.dataset.block = block.id;
    row.style.paddingLeft = `${8 + depth * 16}px`;
    row.append(textElement("span", blockLabel(block), "row-label"));
    list.push(row);
  });
  outline.replaceChildren(...list);
  markOutline();
  if (focusId !== undefined) rowFor(focusId)?.focus({ preventScroll: true });
}

/** The selected row, the one tab stop of the tree, and a dot on each block with an issue. */
function markOutline(): void {
  const flagged = new Set(issues.map(issue => issue.block));
  const all = rows();
  const stop = all.find(row => row.dataset.block === selected) ?? all[0];
  for (const row of all) {
    row.setAttribute("aria-selected", String(row.dataset.block === selected));
    row.tabIndex = row === stop ? 0 : -1;
    const dot = row.querySelector(".dot");
    if (flagged.has(row.dataset.block) && !dot) {
      const mark = textElement("span", "", "dot");
      mark.setAttribute("role", "img");
      mark.setAttribute("aria-label", C.issues);
      row.append(mark);
    } else if (!flagged.has(row.dataset.block)) dot?.remove();
  }
}

function drawBlock(): void {
  const place = layout() ? selectedPlace() : undefined;
  blockGroup.hidden = !place;
  if (!place || !email || !isLayoutEmail(email)) return;
  const { block } = place;
  blockTitle.textContent = C.blockNames[block.type] ?? "";
  moveUp.hidden = !canMove(email, block.id, -1);
  moveDown.hidden = !canMove(email, block.id, 1);
  const fields: readonly string[] = EDITABLE_FIELDS[block.type] ?? [];
  for (const row of fieldRows) row.hidden = row.dataset.field !== "brand" && !fields.includes(row.dataset.field ?? "");
  for (const field of blockFields) refresh(field);
  const pictured = str(block.src) !== "";
  byId("picture-none").hidden = pictured;
  byId("picture-remove").hidden = !pictured;
  paintSwatch("color");
  paintSwatch("background");
}

/**
 * Selects a block (or nothing). Typing waiting for its pause is sent first;
 * typing that cannot be sent goes with the block it was for.
 */
function select(id: string | undefined, options: { post?: boolean; preview?: boolean; fields?: boolean } = {}): void {
  pendingSelect = undefined;
  if (id !== selected) {
    leaveBlock();
    selected = id;
    closeMenu(false);
    markOutline();
    markFrame();
    drawBlock();
    if (options.post !== false) postViewState();
  }
  // Brings the block into view in the preview, or its fields in the inspector. Not every DOM can scroll an element into view (a test's cannot).
  if (options.preview) revealInPreview();
  if (options.fields && !blockGroup.hidden) blockGroup.scrollIntoView?.({ block: "nearest" });
}

function leaveBlock(): void {
  for (const field of blockFields) if (timers.has(field)) commit(field, false);
  for (const field of blockFields) {
    const key = field.key();
    if (key !== undefined) dirty.delete(key);
    tell(field, "");
  }
}

function focusFirstField(): void {
  fieldRows.find(row => !row.hidden)?.querySelector<HTMLElement>("input, textarea, select, button")?.focus();
}

outline.addEventListener("click", event => {
  const row = rowOf(event.target);
  if (row) select(row.dataset.block, { preview: true });
});

outline.addEventListener("keydown", event => {
  const row = rowOf(event.target);
  if (!row) return;
  const all = rows(), at = all.indexOf(row), id = row.dataset.block;
  if (event.altKey && (event.key === "ArrowUp" || event.key === "ArrowDown")) {
    event.preventDefault();
    if (id !== undefined) moveBlock(id, event.key === "ArrowUp" ? -1 : 1);
    return;
  }
  if (event.key === "Enter") {
    event.preventDefault();
    select(id);
    focusFirstField();
    return;
  }
  const next = event.key === "ArrowDown" ? all[at + 1] : event.key === "ArrowUp" ? all[at - 1] : event.key === "Home" ? all[0] : event.key === "End" ? all[all.length - 1] : undefined;
  if (!next) return;
  event.preventDefault();
  select(next.dataset.block, { preview: true });
  next.focus();
});

function moveBlock(id: string, by: -1 | 1): void {
  if (email && layout() && isLayoutEmail(email) && canMove(email, id, by)) post({ type: "edit", op: { kind: "move", block: id, by } });
}
moveUp.addEventListener("click", () => { if (selected !== undefined) moveBlock(selected, -1); });
moveDown.addEventListener("click", () => { if (selected !== undefined) moveBlock(selected, 1); });
removeBlock.addEventListener("click", () => {
  if (selected === undefined || !layout()) return;
  removing = selected;
  post({ type: "edit", op: { kind: "remove", block: selected } });
});

// Add after: a menu of the leaf blocks.
function openMenu(): void {
  addMenu.hidden = false;
  addAfter.setAttribute("aria-expanded", "true");
  // The menu hangs below its button, so near the foot of the inspector part of it would be cut off: the inspector scrolls to show it all.
  addMenu.scrollIntoView?.({ block: "nearest" });
  menuItems[0]?.focus({ preventScroll: true });
}
function closeMenu(focusButton: boolean): void {
  if (addMenu.hidden) return;
  addMenu.hidden = true;
  addAfter.setAttribute("aria-expanded", "false");
  if (focusButton) addAfter.focus();
}
addAfter.addEventListener("click", () => addMenu.hidden ? openMenu() : closeMenu(true));
addMenu.addEventListener("keydown", event => {
  const at = menuItems.indexOf(document.activeElement as HTMLButtonElement);
  if (event.key === "Escape") { event.preventDefault(); closeMenu(true); return; }
  if (event.key === "Tab") { closeMenu(false); return; }
  const next = event.key === "ArrowDown" ? menuItems[(at + 1) % menuItems.length] : event.key === "ArrowUp" ? menuItems[(at - 1 + menuItems.length) % menuItems.length] : event.key === "Home" ? menuItems[0] : event.key === "End" ? menuItems[menuItems.length - 1] : undefined;
  if (!next) return;
  event.preventDefault();
  next.focus();
});
for (const item of menuItems) item.addEventListener("click", () => {
  if (selected === undefined || !layout()) return;
  awaitingAdd = true;
  post({ type: "edit", op: { kind: "add", after: selected, type: item.dataset.type as AddableType } });
  closeMenu(true);
});
document.addEventListener("click", event => {
  if (!addMenu.hidden && event.target instanceof Node && !addMenu.contains(event.target) && !addAfter.contains(event.target)) closeMenu(false);
});

// Pictures: the host chooses, uploads or removes; the page only says which block.
for (const [id, action] of [["picture-choose", "choose"], ["picture-upload", "upload"], ["picture-remove", "remove"]] as const) {
  byId(id).addEventListener("click", () => {
    if (selected === undefined) return;
    clearBarNotice();
    post({ type: "picture", block: selected, action });
  });
}

// -- Issues ---------------------------------------------------------------------

function drawIssues(): void {
  const shown = email && invalid === undefined ? issues : [];
  issuesGroup.hidden = shown.length === 0;
  issueList.replaceChildren(...shown.map(issue => {
    const item = document.createElement("li");
    const block = issue.block;
    if (block !== undefined && placed.has(block)) {
      const button = textElement("button", issue.message, "text-act") as HTMLButtonElement;
      button.type = "button";
      button.addEventListener("click", () => select(block, { preview: true, fields: true }));
      item.append(button);
    } else item.append(textElement("span", issue.message, "issue-line"));
    return item;
  }));
  markOutline();
}

// -- Samples and the page state ------------------------------------------------

const sampleInputs = new Map<string, HTMLInputElement>();
let sampleTimer: number | undefined;

/** One input per merge tag, in the tags' order; inputs whose tags stay are kept, with what is typed in them. */
function drawSamples(): void {
  const keys = email && invalid === undefined ? sampleKeys(email) : [];
  samplesBox.hidden = keys.length === 0;
  if (same(keys, [...sampleInputs.keys()])) return;
  const active = holds(sampleFields) ? document.activeElement as HTMLElement : undefined;
  const inputs = keys.map(key => sampleInputs.get(key) ?? sampleInput(key));
  sampleInputs.clear();
  keys.forEach((key, index) => sampleInputs.set(key, inputs[index]!));
  sampleFields.replaceChildren(...inputs.map(input => input.parentElement!));
  active?.focus();
}

function sampleInput(key: string): HTMLInputElement {
  const label = textElement("label", "", "field");
  const input = document.createElement("input");
  input.dataset.sample = key;
  input.placeholder = C.sampleValue;
  input.maxLength = 2000;
  input.autocomplete = "off";
  input.value = samples[key] ?? "";
  input.addEventListener("input", () => {
    if (input.value) samples[key] = input.value; else delete samples[key];
    window.clearTimeout(sampleTimer);
    sampleTimer = window.setTimeout(postViewState, PAUSE);
  });
  label.append(textElement("span", Object.hasOwn(C.sampleLabels, key) ? C.sampleLabels[key as keyof typeof C.sampleLabels] : key, "label"), input);
  return input;
}

function viewState(): ViewState {
  const state: ViewState = { width };
  // A selection still waiting for its block is the one to come back to.
  const chosen = pendingSelect?.id ?? selected;
  if (chosen !== undefined) state.selected = chosen;
  if (themeOpen) state.themeOpen = true;
  if (samplesOpen) state.samplesOpen = true;
  if (Object.keys(samples).length) state.samples = { ...samples };
  return state;
}

function postViewState(): void {
  const state = viewState();
  api.setState(state);
  post({ type: "viewState", state });
}

/** A saved state, read as data: anything that is not what it should be is left out. */
function asViewState(value: unknown): Partial<ViewState> {
  if (!isRecord(value)) return {};
  const state: Partial<ViewState> = {};
  if (value.width === "desktop" || value.width === "mobile" || value.width === "plain") state.width = value.width;
  if (typeof value.selected === "string") state.selected = value.selected;
  if (typeof value.themeOpen === "boolean") state.themeOpen = value.themeOpen;
  if (typeof value.samplesOpen === "boolean") state.samplesOpen = value.samplesOpen;
  if (isRecord(value.samples)) state.samples = Object.fromEntries(Object.entries(value.samples).filter((entry): entry is [string, string] => typeof entry[1] === "string"));
  return state;
}

/** Applies a saved state without sending it back. */
function restore(state: Partial<ViewState>): void {
  if (state.width) width = state.width;
  if (state.themeOpen !== undefined) { themeOpen = state.themeOpen; theme.open = themeOpen; }
  if (state.samplesOpen !== undefined) { samplesOpen = state.samplesOpen; samplesBox.open = samplesOpen; }
  if (state.samples) {
    samples = { ...state.samples };
    for (const [key, input] of sampleInputs) if (!holds(input)) input.value = samples[key] ?? "";
  }
  if (state.selected !== undefined) {
    if (placed.has(state.selected)) select(state.selected, { post: false });
    else pendingSelect = { id: state.selected, post: false, focusField: false };
  }
  drawStage();
  api.setState(viewState());
}

// A disclosure opened or closed by the person; a restore sets the flag first, so its toggle is not sent back.
theme.addEventListener("toggle", () => { if (theme.open !== themeOpen) { themeOpen = theme.open; postViewState(); } });
samplesBox.addEventListener("toggle", () => { if (samplesBox.open !== samplesOpen) { samplesOpen = samplesBox.open; postViewState(); } });

// -- The action bar ---------------------------------------------------------------

const isAct = (value: unknown): value is BarAct => typeof value === "string" && Object.hasOwn(ACT_WORDS, value);

/** The state's words (a live region: written only when they change) and the acts, kept as they are when they are the same. */
function drawBar(): void {
  if (barState.textContent !== bar.state) barState.textContent = bar.state;
  barState.classList.toggle("busy", BUSY.includes(bar.state));
  const shown = Array.from(barActs.querySelectorAll<HTMLElement>("button"));
  if (same(shown.map(button => button.dataset.act), bar.acts)) return;
  const active = document.activeElement;
  const focused = holds(barActs) && active instanceof HTMLElement ? active.dataset.act : undefined;
  barActs.replaceChildren(...bar.acts.map(act => shown.find(button => button.dataset.act === act) ?? actButtonFor(act)));
  if (focused !== undefined) actButton(focused)?.focus();
}

function actButtonFor(act: BarAct): HTMLElement {
  const button = textElement("button", ACT_WORDS[act], BUTTON_ACTS.includes(act) ? "primary" : "text-act") as HTMLButtonElement;
  button.type = "button";
  button.dataset.act = act;
  button.addEventListener("click", () => runAct(act));
  return button;
}

const actButton = (act: string) => Array.from(barActs.querySelectorAll<HTMLElement>("button")).find(button => button.dataset.act === act);

function runAct(act: BarAct): void {
  if (actPending) return;
  clearBarNotice();
  if (act === "saveLayout") { openForm(); return; }
  actPending = true;
  post({ type: "act", act });
  // The act's button may go with the host's answer: the focus waits on the state, not on the page.
  barState.focus();
}

function openForm(): void {
  barState.hidden = true;
  barActs.hidden = true;
  barForm.hidden = false;
  layoutName.value = bar.layoutName ?? "";
  layoutName.focus();
  layoutName.select();
}

function closeForm(focusAct: boolean): void {
  if (barForm.hidden) return;
  barForm.hidden = true;
  barState.hidden = false;
  barActs.hidden = false;
  if (focusAct) actButton("saveLayout")?.focus();
}

function saveLayout(): void {
  const name = layoutName.value.trim();
  if (name === "" || characters(name) > 200 || /[\u0000-\u001f\u007f-\u009f]/.test(name)) {
    showProblem({ message: EMAIL_COPY.host.layoutNameInvalid });
    return;
  }
  if (actPending) return;
  clearBarNotice();
  actPending = true;
  post({ type: "saveLayout", name });
  closeForm(false);
  barState.focus();
}

layoutName.addEventListener("keydown", event => {
  if (event.key === "Enter") { event.preventDefault(); saveLayout(); }
  if (event.key === "Escape") { event.preventDefault(); closeForm(true); }
});
byId("layout-save").addEventListener("click", saveLayout);
byId("layout-cancel").addEventListener("click", () => closeForm(true));

function showProblem(problem: Problem): void {
  renderProblem(barNotice, { message: problem.message || C.renderFailed, reference: problem.reference }, post);
  barNotice.hidden = false;
}
function clearBarNotice(): void {
  barNotice.hidden = true;
  barNotice.replaceChildren();
}

// -- The rest of the page ---------------------------------------------------------

byId("view-source").addEventListener("click", () => post({ type: "mode", mode: "source" }));
byId("edit-html").addEventListener("click", () => post({ type: "mode", mode: "split" }));
const views = Array.from(document.querySelectorAll<HTMLButtonElement>(".toolbar .views:not(.widths) button"));
views.forEach((button, index) => button.addEventListener("keydown", event => {
  const next = event.key === "ArrowRight" ? views[(index + 1) % views.length] : event.key === "ArrowLeft" ? views[(index + views.length - 1) % views.length] : undefined;
  if (next) { event.preventDefault(); next.focus(); }
}));
organization.addEventListener("change", () => { if (organization.value) post({ type: "organization", id: organization.value }); });

// -- Messages from the host --------------------------------------------------------

/** The editing groups the email has: the layout's outline and theme, or the line of a custom email. */
function drawGroups(): void {
  emailGroup.hidden = !email || invalid !== undefined;
  blocksGroup.hidden = !layout();
  theme.hidden = !layout();
  customNote.hidden = !custom();
  customWords.textContent = htmlless() ? C.plainOnly : C.htmlOnly;
}

function onDocument(message: Record<string, unknown>): void {
  // A host update moves the focus only when the person is in the page.
  const focused = document.hasFocus() ? document.activeElement : null;
  if (typeof message.version === "number") version = message.version;
  for (const [key, entry] of sent) if (version > entry.version) sent.delete(key);
  if (!isRecord(message.email)) {
    invalid = str(message.invalid) || C.invalidJson;
    stale = "invalid";
    renderProblem(previewNotice, { message: invalid }, post);
    previewNotice.hidden = false;
    closeMenu(false);
    drawGroups(); drawBlock(); drawIssues(); drawSamples(); drawStage();
    post({ type: "shown", version, subject: "", state: bar.state, blocks: 0 });
    return;
  }
  if (invalid !== undefined) {
    invalid = undefined;
    if (stale === "invalid") { stale = undefined; previewNotice.hidden = true; previewNotice.replaceChildren(); }
  }
  const previousOrder = order, previous = selected;
  email = message.email as unknown as EmailDocument;
  placed = new Map();
  order = [];
  if (isLayoutEmail(email)) walkBlocks(email.blocks, (block, _depth, _siblings, _index, inBrand) => {
    if (!placed.has(block.id)) placed.set(block.id, { block, brand: inBrand });
    order.push(block.id);
  });
  // The selection follows the document: to the block asked for, or, when its block went, to the one now in its place.
  let next = selected, posted = false, focusField = false;
  if (next !== undefined && !placed.has(next)) {
    next = order[Math.min(Math.max(previousOrder.indexOf(next), 0), order.length - 1)];
    posted = true;
  }
  if (pendingSelect && placed.has(pendingSelect.id)) {
    if (next !== pendingSelect.id) { next = pendingSelect.id; posted = posted || pendingSelect.post; }
    focusField = pendingSelect.focusField;
    pendingSelect = undefined;
  }
  if (next !== selected) { leaveBlock(); selected = next; closeMenu(false); }
  drawGroups();
  refresh(subjectField);
  refresh(preheaderField);
  drawOutline();
  drawBlock();
  for (const field of themeFields) refresh(field);
  drawSamples();
  drawIssues();
  markFrame();
  drawStage();
  if (posted) postViewState();
  // Focus stays where it was, unless what had it went: then it goes to the selected block's row.
  if (focusField && focused) focusFirstField();
  else if (removing !== undefined && removing === previous && !placed.has(removing) && focused && (blockGroup.contains(focused) || !reachable(focused))) rowFor(selected)?.focus();
  else if (focused && focused !== document.body && !reachable(focused)) rowFor(selected)?.focus();
  if (removing !== undefined && !placed.has(removing)) removing = undefined;
  post({ type: "shown", version, subject: str(email.subject), state: bar.state, blocks: order.length });
}

function onRender(message: Record<string, unknown>): void {
  if (typeof message.seq !== "number" || message.seq < lastSeq) return;
  lastSeq = message.seq;
  issues = (Array.isArray(message.issues) ? message.issues : []).filter((issue): issue is Issue => isRecord(issue) && typeof issue.message === "string" && (issue.block === undefined || typeof issue.block === "string"));
  renderText = str(message.text);
  loadRender(str(message.html), message.seq);
  drawIssues();
  drawStage();
}

function onRenderFailed(message: Record<string, unknown>): void {
  if (typeof message.seq !== "number" || message.seq < lastSeq || !isRecord(message.problem)) return;
  lastSeq = message.seq;
  failedSeq = message.seq;
  stale = "failed";
  renderProblem(previewNotice, { message: str(message.problem.message) || C.renderFailed, reference: typeof message.problem.reference === "string" ? message.problem.reference : undefined }, post);
  previewNotice.hidden = false;
  drawStage();
}

function onContext(message: Record<string, unknown>): void {
  connected = message.connected === true;
  // A context answers an act too (a Connect that was cancelled).
  actPending = false;
  // Without a connection the last render may no longer be the email: the next one shows when it loads.
  if (!connected) frameShown = false;
  const choices = (Array.isArray(message.choices) ? message.choices : []).filter((choice): choice is { id: string; name: string } => isRecord(choice) && typeof choice.id === "string" && typeof choice.name === "string");
  const chosen = isRecord(message.organization) ? str(message.organization.id) : "";
  organizationKnown = chosen !== "";
  organizationRow.hidden = choices.length === 0;
  if (choices.length && !holds(organization)) {
    const options = choices.map(choice => { const option = textElement("option", choice.name) as HTMLOptionElement; option.value = choice.id; return option; });
    if (!chosen) { const prompt = textElement("option", C.chooseOrganization) as HTMLOptionElement; prompt.value = ""; options.unshift(prompt); }
    organization.replaceChildren(...options);
    organization.value = chosen;
  }
  drawStage();
}

function onBar(message: Record<string, unknown>): void {
  bar = { state: str(message.state), acts: (Array.isArray(message.acts) ? message.acts : []).filter(isAct), layoutName: typeof message.layoutName === "string" ? message.layoutName : undefined };
  actPending = false;
  if (!bar.acts.includes("saveLayout")) closeForm(false);
  drawBar();
}

function onSelect(message: Record<string, unknown>): void {
  if (typeof message.block !== "string") return;
  const focusField = awaitingAdd;
  awaitingAdd = false;
  if (!placed.has(message.block)) { pendingSelect = { id: message.block, post: true, focusField }; return; }
  select(message.block, { preview: true, fields: true });
  if (focusField && document.hasFocus()) focusFirstField();
}

window.addEventListener("message", event => {
  // VS Code relays host messages from this webview's own origin; a message from any other origin is a frame posting into this window.
  if (event.origin !== window.origin) return;
  const message: unknown = event.data;
  if (!isRecord(message)) return;
  switch (message.type) {
    case "document": onDocument(message); break;
    case "render": onRender(message); break;
    case "renderFailed": onRenderFailed(message); break;
    case "context": onContext(message); break;
    case "bar": onBar(message); break;
    case "restore": restore(asViewState(message.state)); break;
    case "select": onSelect(message); break;
    case "problem":
      // The act, the selection, the add or the removal the problem answers is over.
      actPending = false;
      pendingSelect = undefined;
      awaitingAdd = false;
      removing = undefined;
      if (isRecord(message.problem)) showProblem({ message: str(message.problem.message), reference: typeof message.problem.reference === "string" ? message.problem.reference : undefined });
      break;
  }
});

restore(asViewState(api.getState()));
drawBar();
post({ type: "ready" });
