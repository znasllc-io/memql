// The New email page, running inside the webview. It draws what the host sends
// (the organizations and their layouts, a render of each layout, the
// references and the writing's progress) and sends back what the person does:
// the organization, the layout, the name and the description as a draft the
// host keeps, the references' switches and removals, dictation, and the acts.
// The markup is newEmailPage.ts's.
//
// Host messages count only from this webview's own origin, and every field is
// checked before use. Nothing from a message becomes markup except through
// previewHTML (a layout's render, in a frame with no sandbox tokens) and
// skeletonHTML (numbers only); names and labels go in as text, and layout ids
// are only ever compared, never written into a selector.
//
// A host update moves the focus only while the person is in the page, and only
// when what had it went away.

import { EMAIL_COPY, fill } from "./emailCopy.js";
import type { EmailBlock } from "./emailDocument.js";
import { previewHTML, skeletonHTML } from "./emailPreview.js";
import { emailNameProblem } from "./newEmailPage.js";
import { ICONS } from "./pageBase.js";
import type { Problem } from "./problems.js";
import { renderProblem } from "./problemView.js";

declare function acquireVsCodeApi(): { postMessage(message: unknown): void; getState(): unknown; setState(state: unknown): void };

type Act = "connect" | "retry" | "cancel" | "start" | "write";
type Phase = "starting" | "listening" | "transcribing" | "idle";
interface Organization { id: string; name: string }
interface Layout { id: string; name: string; description: string; kind: "builtin" | "organization"; blocks: EmailBlock[]; key: string }
interface Setup { connected: boolean; domain: string; organizations: Organization[]; organizationId?: string; layouts: Layout[]; dictation: boolean }
interface Reference { index: number; label: string; kind: "picture" | "bundle" | "file"; include: boolean; uploading: boolean }
interface Draft { organizationId?: string; layoutId?: string; name?: string; description?: string }
/** What the page is: which parts show, the state in words and the acts legal from it. */
interface View { kind: "notConnected" | "loading" | "layoutsFailed" | "noOrganization" | "writing" | "form"; state: string; acts: Act[]; busy?: boolean; quiet?: boolean; notice?: Problem }

const C = EMAIL_COPY.newEmail;
const ACT_WORDS: Readonly<Record<Act, string>> = { connect: C.connect, retry: C.retry, cancel: C.cancel, start: C.start, write: C.write };
/** The description's limit, in UTF-16 code units as the field's maxlength counts. */
const DESCRIPTION_LIMIT = 8000;
/** The shape drawn while an email is written in a layout the page does not know. */
const SOME_EMAIL: EmailBlock[] = [{ id: "a", type: "heading" }, { id: "b", type: "text", text: " ".repeat(210) }, { id: "c", type: "button" }];
// The drawings the shared set lacks: a file, and the square that stops a dictation. Constant markup, nothing from a message.
const svg = (shape: string) => `<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${shape}</svg>`;
const FILE_ICON = svg('<path d="M6 3h8l4 4v14H6z"/><path d="M14 3v4h4"/>');
const STOP_ICON = svg('<rect x="6" y="6" width="12" height="12" rx="2"/>');

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
/** Whether a person can reach the element: on the page and not inside anything hidden. */
const reachable = (element: Element) => element.isConnected && !element.closest("[hidden]");
/**
 * Whether the person is working in `box` now. The page keeps its active element
 * while another window has the keyboard, so the window's focus is asked as well.
 */
const holds = (box: Element) => document.hasFocus() && box.contains(document.activeElement);

/**
 * Puts `children` in `parent`, in order, moving only what has to move: what
 * goes is removed first, so an element that stays is not taken out and put
 * back, and keeps its focus.
 */
function reconcile(parent: Element, children: readonly Element[]): void {
  const keep = new Set(children);
  for (const child of Array.from(parent.children)) if (!keep.has(child)) child.remove();
  children.forEach((child, index) => { if (parent.children[index] !== child) parent.insertBefore(child, parent.children[index] ?? null); });
}

// -- The page -----------------------------------------------------------------

const meta = byId("meta"), noOrganization = byId("no-organization"), form = byId("form");
const organizationRow = byId("organization-row"), organizationSelect = byId<HTMLSelectElement>("organization");
const layoutGroup = byId("layout-group"), layoutsBox = byId("layouts"), placeholders = byId("placeholders");
const sets = {
  organization: { box: byId("set-organization"), head: byId("set-organization-head"), cards: byId("cards-organization") },
  builtin: { box: byId("set-builtin"), head: byId("set-builtin-head"), cards: byId("cards-builtin") },
};
const nameInput = byId<HTMLInputElement>("name"), nameNotice = byId("name-notice");
const descriptionInput = byId<HTMLTextAreaElement>("description");
const attach = byId("attach"), dictate = byId("dictate"), referencesBox = byId("references");
const dictationStatus = byId("dictation-status"), describeNotice = byId("describe-notice");
const writingBox = byId("writing");
const barNotice = byId("bar-notice"), barState = byId("bar-state"), barActs = byId("bar-acts");

// The bar and the problem above it stay at the foot of the window: the scroll margin of the page's controls keeps one
// the keyboard reaches above them, and reads their height from --dock. render() measures it whenever it draws them,
// and a resize (the problem wrapping in a narrower window) measures it again.
const dock = barNotice.parentElement!;
const measureDock = () => document.documentElement.style.setProperty("--dock", `${Math.ceil(dock.getBoundingClientRect().height)}px`);
if (typeof ResizeObserver !== "undefined") new ResizeObserver(measureDock).observe(dock);

// -- What the page knows -------------------------------------------------------

/** The host's last word on the organizations and their layouts. */
let setup: Setup | undefined;
/** Waiting for a list of layouts: from the start, and after asking for another organization's or for a retry. */
let waiting = true;
let layoutsProblem: Problem | undefined;
/** The organization the page shows as chosen: the host's, or the one the person picked and the page asked for. */
let organization: string | undefined;
/** An organization from a restored draft that the host has not listed layouts for: asked for once. */
let wantedOrganization: string | undefined;
let chosen: string | undefined;
/** The layout being written, while the host writes ("" when it did not say). */
let writing: string | undefined;
/** The last write that failed: its state and problem stay until the person changes something. */
let failure: Problem | undefined;
/** An act was sent; the next waits for the host's answer. */
let pending: Act | undefined;
/** The host restores the draft once a page: a later restore is the host repeating itself. */
let restored = false;
let phase: Phase = "idle";
/** What the description held when this page started dictating: a transcript is written after it. */
let dictationBase: string | undefined;
/** What the notice under the description is about: the references or the dictation. */
let describeAbout: "references" | "dictation" | undefined;
let shownState: string | undefined;
let noticeShown: Problem | undefined;
let writingShape: string | undefined;
/** The cards by layout (id, words and shape): a card whose layout comes back keeps its render. */
let cardCache = new Map<string, HTMLElement>();
let cardCount = 0;
let referenceCount = 0;

const layouts = () => setup?.layouts ?? [];
const chosenLayout = () => waiting ? undefined : layouts().find(layout => layout.id === chosen);
const allCards = () => Array.from(layoutsBox.querySelectorAll<HTMLElement>('[role="radio"]'));
/** Only words count as a description. */
const described = () => descriptionInput.value.trim() !== "";

// -- The state ------------------------------------------------------------------

function view(): View {
  if (setup && !setup.connected) return { kind: "notConnected", state: setup.domain ? fill(C.notConnected, { domain: setup.domain }) : EMAIL_COPY.editor.notConnected, acts: ["connect"] };
  if (waiting) return { kind: "loading", state: C.loading, acts: [], quiet: true };
  if (layoutsProblem || !setup) return { kind: "layoutsFailed", state: C.layoutsFailed, acts: ["retry"], notice: layoutsProblem };
  if (setup.organizations.length === 0) return { kind: "noOrganization", state: C.noOrganization, acts: [] };
  if (writing !== undefined) return { kind: "writing", state: C.writing, acts: ["cancel"], busy: true };
  if (!organization) return { kind: "form", state: C.chooseOrganization, acts: [] };
  if (!chosenLayout()) return { kind: "form", state: C.chooseLayout, acts: [] };
  if (emailNameProblem(nameInput.value)) return { kind: "form", state: C.nameIt, acts: [] };
  const acts: Act[] = described() ? ["start", "write"] : ["start"];
  return failure ? { kind: "form", state: C.failed, acts, notice: failure } : { kind: "form", state: C.ready, acts };
}

/** Draws the parts the state decides: which regions show, the problem above the bar, and the bar. */
function render(): void {
  const now = view();
  form.hidden = now.kind === "notConnected" || now.kind === "noOrganization" || now.kind === "writing";
  layoutGroup.hidden = now.kind === "layoutsFailed";
  noOrganization.hidden = now.kind !== "noOrganization";
  writingBox.hidden = now.kind !== "writing";
  organizationRow.hidden = (setup?.organizations.length ?? 0) < 2;
  const only = setup?.connected && setup.organizations.length === 1 ? setup.organizations[0]!.name : undefined;
  meta.hidden = only === undefined;
  if (meta.textContent !== (only ?? "")) meta.textContent = only ?? "";
  if (now.kind === "writing") drawWriting();
  drawNotice(now.notice);
  drawBar(now);
  measureDock();
}

function drawNotice(problem: Problem | undefined): void {
  const shown = problem && problem.message ? problem : undefined;
  if (shown === noticeShown) return;
  noticeShown = shown;
  barNotice.hidden = !shown;
  if (shown) renderProblem(barNotice, shown, post);
  else barNotice.replaceChildren();
}

// -- The action bar ---------------------------------------------------------------

/** Write it, Connect and Retry are buttons and Cancel a text act; Start from the layout is the button when it is the only act. */
const actStyle = (act: Act, acts: readonly Act[]) => act === "cancel" || act === "start" && acts.length > 1 ? "text-act" : "primary";

/** The state's words (a live region: written only when they change) and the acts, kept as they are when they are the same. */
function drawBar(now: View): void {
  if (shownState !== now.state) {
    shownState = now.state;
    // Loading has no words on the screen: its shape says it. A screen reader still hears them.
    if (now.quiet) barState.replaceChildren(textElement("span", now.state, "sr-only"));
    else barState.textContent = now.state;
    post({ type: "shown", state: now.state, loading: now.kind === "loading" });
  }
  barState.classList.toggle("busy", now.busy === true);
  const shown = Array.from(barActs.querySelectorAll<HTMLElement>("button"));
  reconcile(barActs, now.acts.map(act => {
    const style = actStyle(act, now.acts);
    return shown.find(button => button.dataset.act === act && button.classList.contains(style)) ?? actButton(act, style);
  }));
}

function actButton(act: Act, style: string): HTMLElement {
  const button = textElement("button", ACT_WORDS[act], style) as HTMLButtonElement;
  button.type = "button";
  button.dataset.act = act;
  button.addEventListener("click", () => runAct(act));
  return button;
}

function runAct(act: Act): void {
  if (pending || !view().acts.includes(act)) return;
  const layoutId = chosen!, name = nameInput.value.trim();
  switch (act) {
    case "start":
    case "write":
      // The description goes as it stands: a dictation still running stops here.
      stopDictating();
      failure = undefined;
      post(act === "start" ? { type: "start", layoutId, name } : { type: "write", layoutId, name, description: descriptionInput.value.trim() });
      break;
    case "retry":
      layoutsProblem = undefined;
      waiting = true;
      post({ type: "retry" });
      drawLayouts();
      break;
    case "cancel":
    case "connect":
      post({ type: act });
      break;
  }
  pending = act;
  // The act's button may go with the host's answer: the focus waits on the state, not on the page.
  barState.focus();
  render();
}

// -- Organizations ------------------------------------------------------------------

function drawOrganizations(): void {
  // A choice being made stays as the person has it.
  if (holds(organizationSelect)) return;
  const choices = setup?.organizations ?? [];
  const wanted: [string, string][] = choices.map(choice => [choice.id, choice.name]);
  if (!organization) wanted.unshift(["", C.chooseOrganization]);
  const options = Array.from(organizationSelect.options);
  if (options.length !== wanted.length || options.some((option, index) => option.value !== wanted[index]![0] || option.textContent !== wanted[index]![1])) {
    organizationSelect.replaceChildren(...wanted.map(([value, name]) => { const option = textElement("option", name) as HTMLOptionElement; option.value = value; return option; }));
  }
  organizationSelect.value = organization ?? "";
}

/** Asks the host for an organization's layouts; until they come, the page shows their shape. */
function askFor(id: string): void {
  organization = id;
  waiting = true;
  post({ type: "organization", id });
}

organizationSelect.addEventListener("change", () => {
  const id = organizationSelect.value;
  if (!id || id === organization || !setup?.organizations.some(choice => choice.id === id)) return;
  wantedOrganization = undefined;
  askFor(id);
  changed();
  saveDraft();
  drawLayouts();
  render();
});

organizationSelect.addEventListener("blur", drawOrganizations);

// -- Layouts --------------------------------------------------------------------------

function cardFor(layout: Layout): HTMLElement {
  const count = ++cardCount;
  const card = document.createElement("div");
  card.className = "card";
  card.setAttribute("role", "radio");
  card.setAttribute("aria-checked", "false");
  card.tabIndex = -1;
  card.dataset.id = layout.id;
  // The render, or the layout's shape until it comes: a picture of the card, which a screen reader skips for its name.
  const thumb = textElement("div", "", "thumb"), canvas = textElement("div", "", "canvas"), shape = textElement("div", "", "shape");
  thumb.setAttribute("aria-hidden", "true");
  shape.innerHTML = skeletonHTML(layout.blocks); // numbers only: no text from the layout enters it
  canvas.append(shape);
  thumb.append(canvas);
  const name = textElement("span", layout.name, "card-name");
  name.id = `layout-name-${count}`;
  name.title = layout.name;
  card.setAttribute("aria-labelledby", name.id);
  card.append(thumb, name);
  if (layout.description) {
    const description = textElement("span", layout.description, "card-description");
    description.id = `layout-description-${count}`;
    description.title = layout.description;
    card.setAttribute("aria-describedby", description.id);
    card.append(description);
  }
  card.addEventListener("click", () => choose(card.dataset.id!));
  card.addEventListener("keydown", event => cardKey(card, event));
  return card;
}

/** The cards in their sets (the organization's under its name, then the built-in ones; subheads only when both have cards), or their shape while the list loads. */
function drawLayouts(): void {
  layoutsBox.classList.toggle("loading", waiting);
  placeholders.hidden = !waiting;
  const list = waiting ? [] : layouts();
  const next = new Map<string, HTMLElement>();
  const take = (layout: Layout) => { const card = cardCache.get(layout.key) ?? cardFor(layout); next.set(layout.key, card); return card; };
  const own = list.filter(layout => layout.kind === "organization").map(take);
  const builtIn = list.filter(layout => layout.kind === "builtin").map(take);
  // While the list loads, the cards wait off the page with their renders, in case their layouts come back.
  if (!waiting) cardCache = next;
  reconcile(sets.organization.cards, own);
  reconcile(sets.builtin.cards, builtIn);
  const both = own.length > 0 && builtIn.length > 0;
  for (const [set, cards] of [[sets.organization, own], [sets.builtin, builtIn]] as const) {
    set.box.hidden = cards.length === 0;
    set.head.hidden = !both;
    if (both) { set.box.setAttribute("role", "group"); set.box.setAttribute("aria-labelledby", set.head.id); }
    else { set.box.removeAttribute("role"); set.box.removeAttribute("aria-labelledby"); }
  }
  const owner = setup?.organizations.find(choice => choice.id === setup?.organizationId)?.name ?? C.organization;
  if (sets.organization.head.textContent !== owner) sets.organization.head.textContent = owner;
  markCards();
}

/** The chosen card, and the one tab stop of the group: the chosen card, else the first. */
function markCards(): void {
  const all = allCards();
  const checked = waiting ? undefined : all.find(card => card.dataset.id === chosen);
  for (const card of all) {
    card.setAttribute("aria-checked", String(card === checked));
    card.tabIndex = card === (checked ?? all[0]) ? 0 : -1;
  }
}

function choose(id: string): void {
  if (id === chosen) return;
  chosen = id;
  changed();
  markCards();
  saveDraft();
  render();
}

function cardKey(card: HTMLElement, event: KeyboardEvent): void {
  if (event.altKey || event.ctrlKey || event.metaKey) return;
  const all = allCards(), at = all.indexOf(card);
  let next: HTMLElement | undefined;
  switch (event.key) {
    case "ArrowRight": case "ArrowDown": next = all[(at + 1) % all.length]; break;
    case "ArrowLeft": case "ArrowUp": next = all[(at - 1 + all.length) % all.length]; break;
    case "Home": next = all[0]; break;
    case "End": next = all[all.length - 1]; break;
    case " ": next = card; break;
    default: return;
  }
  event.preventDefault();
  if (!next) return;
  if (next !== card) next.focus();
  choose(next.dataset.id!);
}

/**
 * A layout's render, in its own frame: sandboxed with no tokens (nothing runs,
 * and the page has no way in), cleaned by previewHTML, and shown once it has
 * loaded so the card never goes blank. A render that failed (no HTML) leaves
 * the layout's shape.
 */
function showRender(card: HTMLElement, html: string): void {
  const canvas = card.querySelector(".canvas")!, shape = canvas.querySelector<HTMLElement>(".shape")!;
  const frames = () => Array.from(canvas.querySelectorAll("iframe"));
  if (!html) {
    for (const frame of frames()) frame.remove();
    shape.hidden = false;
    return;
  }
  const frame = document.createElement("iframe");
  frame.setAttribute("sandbox", "");
  frame.tabIndex = -1;
  frame.hidden = true;
  frame.addEventListener("load", () => {
    if (!frame.isConnected) return;
    const all = frames();
    // A newer render is on its way: this one gives way to it.
    if (all[all.length - 1] !== frame) { if (frame.hidden) frame.remove(); return; }
    for (const other of all) if (other !== frame) other.remove();
    frame.hidden = false;
    shape.hidden = true;
  });
  frame.setAttribute("srcdoc", previewHTML(html, { blocks: false }));
  canvas.append(frame);
}

// -- The name and the description ---------------------------------------------------

/** A blank name is only missing (the bar says so); a name that can't be one says why under the field. */
function drawNameNotice(): void {
  const value = nameInput.value;
  const problem = value.trim() ? emailNameProblem(value) ?? "" : "";
  if (nameNotice.textContent !== problem) nameNotice.textContent = problem;
  if (problem) nameInput.setAttribute("aria-invalid", "true");
  else nameInput.removeAttribute("aria-invalid");
}

/** Sets the description, within its limit and never splitting a character. */
function setDescription(value: string): void {
  let text = value;
  if (text.length > DESCRIPTION_LIMIT) {
    const end = DESCRIPTION_LIMIT - (/[\ud800-\udbff]/.test(text[DESCRIPTION_LIMIT - 1]!) ? 1 : 0);
    text = text.slice(0, end);
  }
  descriptionInput.value = text;
}

nameInput.addEventListener("input", () => { changed(); drawNameNotice(); saveDraft(); render(); });
descriptionInput.addEventListener("input", () => { changed(); saveDraft(); render(); });
// Taking the focus scrolls a text field only as far as its caret: the whole description, outline and all, comes above the bar.
descriptionInput.addEventListener("focus", () => descriptionInput.scrollIntoView?.({ block: "nearest" }));
// Ctrl or Cmd+Enter writes, from anywhere in the form, when Write it is legal.
form.addEventListener("keydown", event => {
  if (event.key !== "Enter" || !(event.metaKey || event.ctrlKey)) return;
  event.preventDefault();
  runAct("write");
});

/** The person changed what they ask for: a failure shown for the last request is over. */
function changed(): void {
  failure = undefined;
}

// -- References ----------------------------------------------------------------------------

attach.addEventListener("click", () => { changed(); post({ type: "attach" }); render(); });

function chipFor(item: Reference): HTMLElement {
  const chip = document.createElement("li");
  chip.className = "chip";
  const icon = textElement("span", "", "chip-icon");
  const name = textElement("span", item.label, "chip-name");
  name.title = item.label;
  name.id = `reference-name-${++referenceCount}`;
  chip.append(icon, name);
  if (item.kind !== "file") {
    // Pictures and ZIPs can go into the email itself; off until the person turns it on.
    const label = textElement("label", "", "switch");
    const toggle = document.createElement("input");
    toggle.type = "checkbox";
    toggle.setAttribute("aria-describedby", name.id);
    toggle.setAttribute("role", "switch");
    toggle.addEventListener("change", () => { changed(); post({ type: "include", index: Number(chip.dataset.index), on: toggle.checked }); render(); });
    label.append(toggle, textElement("span", C.useInEmail));
    chip.append(label);
  }
  const remove = textElement("button", "", "icon-button remove") as HTMLButtonElement;
  remove.type = "button";
  remove.innerHTML = ICONS.close; // a constant drawing, no text from the host
  const words = fill(C.removeReference, { name: item.label });
  remove.setAttribute("aria-label", words);
  remove.title = words;
  remove.addEventListener("click", () => { changed(); post({ type: "removeReference", index: Number(chip.dataset.index) }); render(); });
  chip.append(remove);
  return chip;
}

/** Brings a chip up to date: where it is in the host's list, its switch, and a spinner while it uploads. */
function updateChip(chip: HTMLElement, item: Reference): void {
  chip.dataset.index = String(item.index);
  const toggle = chip.querySelector<HTMLInputElement>('input[role="switch"]');
  if (toggle) toggle.checked = item.include;
  const icon = chip.querySelector<HTMLElement>(".chip-icon")!;
  const drawing = item.uploading ? "uploading" : item.kind === "picture" ? "picture" : "file";
  if (icon.dataset.drawing === drawing) return;
  icon.dataset.drawing = drawing;
  if (item.uploading) {
    const spinner = textElement("span", "", "spinner");
    spinner.setAttribute("role", "img");
    spinner.setAttribute("aria-label", C.uploading);
    icon.replaceChildren(spinner);
  } else icon.innerHTML = drawing === "picture" ? ICONS.picture : FILE_ICON; // constant drawings
}

/** The chips, keeping each one whose reference stays (its switch, its focus); the focus of one removed goes to the next. */
function drawReferences(items: readonly Reference[]): void {
  const focused = holds(referencesBox) ? document.activeElement : null;
  const old = Array.from(referencesBox.children) as HTMLElement[];
  const at = focused ? old.findIndex(chip => chip.contains(focused)) : -1;
  const unused = [...old];
  const chips = items.map(item => {
    const key = `${item.kind}\n${item.label}`;
    const reuse = unused.findIndex(chip => chip.dataset.key === key);
    const chip = reuse >= 0 ? unused.splice(reuse, 1)[0]! : chipFor(item);
    chip.dataset.key = key;
    updateChip(chip, item);
    return chip;
  });
  reconcile(referencesBox, chips);
  if (focused && !focused.isConnected) (chips[Math.min(at, chips.length - 1)]?.querySelector<HTMLElement>("button.remove") ?? attach).focus();
}

function showDescribeNotice(about: "references" | "dictation", problem: Problem): void {
  describeAbout = about;
  renderProblem(describeNotice, problem, post);
}

function clearDescribeNotice(about: "references" | "dictation"): void {
  if (describeAbout !== about) return;
  describeAbout = undefined;
  describeNotice.replaceChildren();
}

// -- Dictation -----------------------------------------------------------------------------

function drawDictation(): void {
  const active = phase !== "idle";
  // A dictation under way can always be stopped, even if the host has since said it can't start one.
  dictate.hidden = !active && !setup?.dictation;
  const label = !active ? C.dictate : phase === "listening" ? C.stopDictation : C.cancelDictation;
  if (dictate.getAttribute("aria-label") !== label) {
    dictate.setAttribute("aria-label", label);
    dictate.title = label;
    dictate.innerHTML = !active ? ICONS.mic : phase === "listening" ? STOP_ICON : ICONS.close; // constant drawings
  }
  dictate.setAttribute("aria-pressed", String(active));
  const status = phase === "starting" ? C.openingMicrophone : phase === "listening" ? C.listening : phase === "transcribing" ? C.transcribing : "";
  if (dictationStatus.textContent !== status) dictationStatus.textContent = status;
  // The transcript writes the description: typing would be written over.
  descriptionInput.readOnly = active;
}

dictate.addEventListener("click", () => {
  if (phase === "idle") {
    if (!setup?.dictation) return;
    dictationBase = descriptionInput.value;
    phase = "starting";
    clearDescribeNotice("dictation");
    post({ type: "dictationStart" });
  } else post({ type: phase === "listening" ? "dictationStop" : "dictationCancel" });
  drawDictation();
});

/** Stops a dictation for good: what it wrote stays, and nothing more is written. */
function stopDictating(): void {
  if (phase === "idle") return;
  post({ type: "dictationCancel" });
  phase = "idle";
  dictationBase = undefined;
  drawDictation();
}

// -- The draft --------------------------------------------------------------------------------

function draft(): Draft {
  const value: Draft = {};
  if (organization) value.organizationId = organization;
  if (chosen) value.layoutId = chosen;
  if (nameInput.value) value.name = nameInput.value;
  if (descriptionInput.value) value.description = descriptionInput.value;
  return value;
}

/** Keeps the draft in the webview's state and gives it to the host to keep: every change the person makes, at once. */
function saveDraft(): void {
  const value = draft();
  api.setState({ draft: value });
  post({ type: "draft", draft: value });
}

/** A saved draft, read as data: anything that is not a string is left out. */
function readDraft(value: unknown): Draft {
  const saved: Draft = {};
  if (!isRecord(value)) return saved;
  for (const key of ["organizationId", "layoutId", "name", "description"] as const) if (typeof value[key] === "string") saved[key] = value[key] as string;
  return saved;
}

/** Applies a saved draft without sending it back. A field the person is typing in keeps what they type. */
function applyDraft(saved: Draft): void {
  if (saved.name !== undefined && !holds(nameInput)) nameInput.value = saved.name;
  if (saved.description !== undefined && !holds(descriptionInput)) setDescription(saved.description);
  const wanted = saved.organizationId;
  if (wanted !== undefined && wanted !== organization) {
    // Layouts belong to an organization: another one's are asked for (once), now if the host has listed its organizations.
    if (setup?.connected && !waiting) { if (setup.organizations.some(choice => choice.id === wanted)) askFor(wanted); }
    else wantedOrganization = wanted;
  }
  if (saved.layoutId !== undefined && (waiting || !setup || layouts().some(layout => layout.id === saved.layoutId))) chosen = saved.layoutId;
  api.setState({ draft: draft() });
  drawNameNotice();
  drawOrganizations();
  drawLayouts();
  render();
}

// -- Messages from the host ---------------------------------------------------------------------

function readProblem(value: unknown): Problem | undefined {
  if (!isRecord(value) || typeof value.message !== "string") return undefined;
  return typeof value.reference === "string" && value.reference ? { message: value.message, reference: value.reference } : { message: value.message };
}

function readLayout(value: unknown): Layout | undefined {
  if (!isRecord(value) || typeof value.id !== "string" || !value.id || typeof value.name !== "string" || (value.kind !== "builtin" && value.kind !== "organization")) return undefined;
  const blocks = isRecord(value.document) && Array.isArray(value.document.blocks) ? value.document.blocks as EmailBlock[] : [];
  const description = str(value.description);
  let key: string;
  try { key = JSON.stringify([value.id, value.name, description, value.kind, blocks]); } catch { return undefined; }
  return { id: value.id, name: value.name, description, kind: value.kind, blocks, key };
}

function readSetup(message: Record<string, unknown>): Setup | undefined {
  if (typeof message.connected !== "boolean") return undefined;
  const organizations: Organization[] = [];
  for (const item of Array.isArray(message.organizations) ? message.organizations : []) {
    if (isRecord(item) && typeof item.id === "string" && item.id && typeof item.name === "string" && !organizations.some(choice => choice.id === item.id)) organizations.push({ id: item.id, name: item.name });
  }
  const list: Layout[] = [];
  for (const item of Array.isArray(message.layouts) ? message.layouts : []) {
    const layout = readLayout(item);
    if (layout && !list.some(other => other.id === layout.id)) list.push(layout);
  }
  return { connected: message.connected, domain: str(message.domain), organizations, organizationId: typeof message.organizationId === "string" ? message.organizationId : undefined, layouts: list, dictation: message.dictation === true };
}

function onSetup(message: Record<string, unknown>): void {
  const next = readSetup(message);
  if (!next) return;
  setup = next;
  if (pending === "connect" || pending === "retry") pending = undefined;
  if (!next.connected) { drawOrganizations(); drawDictation(); render(); return; }
  waiting = false;
  layoutsProblem = undefined;
  const listed = next.organizations.some(choice => choice.id === next.organizationId);
  organization = listed ? next.organizationId : next.organizations.length === 1 ? next.organizations[0]!.id : undefined;
  // A restored organization the host listed no layouts for is asked for, once; after that the host has the last word.
  const wanted = wantedOrganization;
  wantedOrganization = undefined;
  if (wanted !== undefined && wanted !== organization && next.organizations.some(choice => choice.id === wanted)) askFor(wanted);
  if (!waiting && chosen !== undefined && !next.layouts.some(layout => layout.id === chosen)) chosen = undefined;
  drawOrganizations();
  drawLayouts();
  drawDictation();
  render();
}

function onReferences(message: Record<string, unknown>): void {
  if (!Array.isArray(message.items)) return;
  const items = message.items.flatMap((item: unknown, index: number): Reference[] => isRecord(item) && typeof item.label === "string" && item.label
    ? [{ index, label: item.label, kind: item.kind === "picture" || item.kind === "bundle" ? item.kind : "file", include: item.include === true, uploading: item.uploading === true }]
    : []);
  drawReferences(items);
  const problem = readProblem(message.problem);
  if (problem && problem.message) showDescribeNotice("references", problem);
  else clearDescribeNotice("references");
}

function onDictation(message: Record<string, unknown>): void {
  const next = message.phase;
  if (next !== "starting" && next !== "listening" && next !== "transcribing" && next !== "idle") return;
  phase = next;
  // A transcript is written only into a dictation this page started, after what was there.
  if (typeof message.text === "string" && dictationBase !== undefined) {
    const base = dictationBase;
    setDescription(base + (base && message.text && !/\s$/.test(base) ? " " : "") + message.text);
    changed();
    saveDraft();
  }
  if (typeof message.error === "string" && message.error) showDescribeNotice("dictation", { message: message.error, ...(typeof message.reference === "string" && message.reference ? { reference: message.reference } : {}) });
  if (phase === "idle") dictationBase = undefined;
  drawDictation();
  render();
}

function onWriting(message: Record<string, unknown>): void {
  writing = typeof message.layoutId === "string" ? message.layoutId : "";
  pending = undefined;
  failure = undefined;
  stopDictating();
  render();
}

/** The shape of the layout being written: the one the host names, else the chosen one. */
function drawWriting(): void {
  const list = layouts();
  const layout = list.find(item => item.id === writing) ?? list.find(item => item.id === chosen);
  const blocks = layout?.blocks ?? SOME_EMAIL;
  const key = layout?.key ?? "";
  if (writingShape === key) return;
  writingShape = key;
  writingBox.innerHTML = skeletonHTML(blocks); // numbers only: no text from the layout enters it
}

function onFailed(message: Record<string, unknown>): void {
  writing = undefined;
  pending = undefined;
  failure = readProblem(message.problem) ?? { message: "" };
  render();
}

function onIdle(): void {
  writing = undefined;
  pending = undefined;
  render();
}

function onLayoutsFailed(message: Record<string, unknown>): void {
  waiting = false;
  pending = undefined;
  layoutsProblem = readProblem(message.problem) ?? { message: "" };
  drawLayouts();
  render();
}

function onRestore(message: Record<string, unknown>): void {
  if (restored || !isRecord(message.draft)) return;
  restored = true;
  applyDraft(readDraft(message.draft));
}

function onThumbnail(message: Record<string, unknown>): void {
  if (typeof message.id !== "string" || typeof message.html !== "string") return;
  const card = allCards().find(item => item.dataset.id === message.id);
  if (card) showRender(card, message.html);
}

/** What had the focus, if the person is in the page; when a host update takes it away, the focus waits on the state. */
function keepFocus(focused: Element | null): void {
  if (!focused || focused === document.body || reachable(focused)) return;
  const now = document.activeElement;
  if (now && now !== document.body && reachable(now)) return;
  barState.focus();
}

window.addEventListener("message", event => {
  // VS Code relays host messages from this webview's own origin; a message from any other origin is a frame posting into this window.
  if (event.origin !== window.origin) return;
  const message: unknown = event.data;
  if (!isRecord(message)) return;
  const focused = document.hasFocus() ? document.activeElement : null;
  switch (message.type) {
    case "setup": onSetup(message); break;
    case "layoutsFailed": onLayoutsFailed(message); break;
    case "thumbnail": onThumbnail(message); break;
    case "references": onReferences(message); break;
    case "writing": onWriting(message); break;
    case "failed": onFailed(message); break;
    case "idle": onIdle(); break;
    case "restore": onRestore(message); break;
    case "dictation": onDictation(message); break;
    default: return;
  }
  keepFocus(focused);
});

post({ type: "ready" });
// The webview's own state brings the draft back at once; the host's restore follows.
const saved = api.getState();
if (isRecord(saved)) applyDraft(readDraft(saved.draft));
drawLayouts();
drawDictation();
render();
