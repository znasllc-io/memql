import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { JSDOM } from "jsdom";
import { emailPage } from "../src/emailPage.js";
import { EMAIL_COPY } from "../src/emailCopy.js";
import { BLOCK_NAMES, BLOCK_TYPES, EDITABLE_FIELDS, THEME_COLOURS } from "../src/emailDocument.js";

const C = EMAIL_COPY.editor;

function fixture(saved?: unknown) {
  const dom = new JSDOM(emailPage({ title: "Welcome.email.json", script: "", nonce: "test" }), { runScripts: "outside-only" });
  // Messages cross a structured clone in the editor; JSON does the same here and brings them into this realm.
  const messages: any[] = [];
  let state = saved;
  // A clock the test moves by hand, so "after a pause" is exact and the suite never sleeps.
  let now = 0, nextTimer = 1;
  const timers = new Map<number, { at: number; run: () => void }>();
  Object.assign(dom.window, {
    // The webview has TextEncoder (the model counts bytes with it); jsdom's window does not.
    TextEncoder,
    setTimeout: (run: () => void, ms = 0) => { const id = nextTimer++; timers.set(id, { at: now + ms, run }); return id; },
    clearTimeout: (id: number) => { timers.delete(id); },
    acquireVsCodeApi: () => ({ postMessage: (m: any) => messages.push(JSON.parse(JSON.stringify(m))), getState: () => state, setState: (value: unknown) => { state = JSON.parse(JSON.stringify(value)); } }),
  });
  // Every focus() call, the page's and the test's: a host update must never add one while the window is elsewhere.
  const focuses: Element[] = [];
  const focus = dom.window.HTMLElement.prototype.focus;
  dom.window.HTMLElement.prototype.focus = function (this: HTMLElement, ...options: [FocusOptions?]) { focuses.push(this); return focus.apply(this, options); };
  dom.window.eval(readFileSync("dist-test/email-view.js", "utf8"));
  const doc = dom.window.document, el = (id: string) => doc.getElementById(id)!;
  // The person clicks into another window (the Source editor in Split): the field gets blur and stays document.activeElement.
  const away = () => { Object.assign(doc, { hasFocus: () => false }); doc.activeElement?.dispatchEvent(new dom.window.FocusEvent("blur")); };
  const back = () => { delete (doc as unknown as { hasFocus?: unknown }).hasFocus; };
  const send = (data: any) => dom.window.dispatchEvent(new dom.window.MessageEvent("message", { data, origin: dom.window.origin }));
  // jsdom ignores srcdoc: write the pending frame's srcdoc into it, then fire its load event.
  const loadPreview = () => {
    const frame = doc.querySelector<HTMLIFrameElement>('iframe[data-pending="1"]')!;
    const d = frame.contentDocument!; d.open(); d.write(frame.getAttribute("srcdoc")!); d.close();
    frame.dispatchEvent(new dom.window.Event("load"));
    return frame;
  };
  const advance = (ms: number) => {
    now += ms;
    for (;;) {
      let due: [number, { at: number; run: () => void }] | undefined;
      for (const entry of timers) if (entry[1].at <= now && (!due || entry[1].at < due[1].at)) due = entry;
      if (!due) return;
      timers.delete(due[0]); due[1].run();
    }
  };
  const type = (target: string | Element, value: string) => {
    const field = (typeof target === "string" ? el(target) : target) as HTMLInputElement;
    field.value = value; field.dispatchEvent(new dom.window.Event("input", { bubbles: true }));
  };
  const key = (target: Element | Document, name: string, init: KeyboardEventInit = {}) => target.dispatchEvent(new dom.window.KeyboardEvent("keydown", { key: name, bubbles: true, cancelable: true, ...init }));
  const row = (id: string) => doc.querySelector<HTMLElement>(`[role="treeitem"][data-block="${id}"]`)!;
  const select = (id: string) => row(id).click();
  const edits = () => messages.filter(m => m.type === "edit").map(m => m.op);
  const visible = (id: string) => !el(id).closest("[hidden]");
  return { dom, doc, el, send, loadPreview, messages, advance, type, key, row, select, edits, visible, focuses, away, back, state: () => state };
}

const theme = { page: "#eef0f3", canvas: "#ffffff", primary: "#2f5d8a", secondary: "#6f93b8", accent: "#d9822b", tint: "#f5f4f0", text: "#2b2b2b", muted: "#8a8a8a", headingFont: "Georgia", bodyFont: "Helvetica" };
const article = { subject: "Hello", theme, blocks: [
  { id: "headline", type: "heading", text: "Hi" },
  { id: "body", type: "section", blocks: [{ id: "intro", type: "text", text: "Hello {{displayName}}" }] },
  { id: "cta", type: "button", label: "Go", href: "https://example.com" },
] };
const rendered = `<html><body><table><tr><td data-block="headline">Hi</td></tr><tr><td data-block="body"><table><tr><td data-block="intro">Hello</td></tr></table></td></tr><tr><td data-block="cta"><a href="https://example.com">Go</a></td></tr></table></body></html>`;
const clone = <T>(value: T): T => JSON.parse(JSON.stringify(value));
// jsdom fires a disclosure's toggle event on a later turn, as browsers do.
const settle = () => new Promise(resolve => setTimeout(resolve, 5));

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
  // The frame marks the selection, the preview holds nothing that navigates, and a click outside any block keeps the selection.
  assert.equal(frame.contentDocument!.querySelector("[data-selected]")?.getAttribute("data-block"), "cta");
  assert.equal(frame.contentDocument!.querySelectorAll("[href],script").length, 0);
  assert.equal(frame.getAttribute("sandbox"), "allow-same-origin");
  const stray = new f.dom.window.MouseEvent("click", { bubbles: true, cancelable: true });
  frame.contentDocument!.body.dispatchEvent(stray);
  assert.equal(stray.defaultPrevented, true);
  assert.equal(f.el("block-title").textContent, "Button");
  // The reverse: choosing in the outline marks the block in the preview.
  f.select("headline");
  assert.equal(frame.contentDocument!.querySelector("[data-selected]")?.getAttribute("data-block"), "headline");
  assert.equal(frame.contentDocument!.querySelectorAll("[data-selected]").length, 1);
  assert.deepEqual(f.messages.filter(m => m.type === "viewState").map(m => m.state.selected), ["cta", "headline"]);
  f.dom.window.close();
});

test("the outline lists the blocks and the first render replaces the skeleton", () => {
  const f = fixture();
  assert.equal(f.messages[0]?.type, "ready");
  f.send({ type: "document", version: 1, email: article });
  assert.equal(f.visible("skeleton"), true);
  assert.ok(f.el("skeleton").querySelector(".sk-email"), "the skeleton is the shape of the blocks");
  const rows = [...f.doc.querySelectorAll<HTMLElement>('#outline [role="treeitem"]')];
  assert.deepEqual(rows.map(row => row.getAttribute("aria-level")), ["1", "1", "2", "1"]);
  assert.deepEqual(rows.map(row => row.getAttribute("data-block")), ["headline", "body", "intro", "cta"]);
  assert.equal(rows[0]!.textContent, "Heading · Hi");
  assert.equal(f.el("outline").getAttribute("role"), "tree");
  // Before the first render the frames are not shown; the first loaded frame takes over.
  f.send({ type: "render", seq: 1, html: rendered, text: "Hi", subject: "Hello", issues: [] });
  assert.equal(f.visible("skeleton"), true, "the skeleton stays until the frame has loaded");
  const frame = f.loadPreview();
  assert.equal(f.visible("skeleton"), false);
  assert.equal(frame.hasAttribute("data-pending"), false);
  assert.ok(frame.classList.contains("front"));
  // The next render loads in the other frame, and the two swap only when it has loaded.
  f.send({ type: "render", seq: 2, html: rendered.replace(">Hi<", ">Hi there<"), text: "Hi there", subject: "Hello", issues: [] });
  const next = f.doc.querySelector<HTMLIFrameElement>('iframe[data-pending="1"]')!;
  assert.notEqual(next, frame);
  assert.ok(frame.classList.contains("front"), "the shown frame stays until the next one is ready");
  f.loadPreview();
  assert.ok(next.classList.contains("front"));
  assert.ok(!frame.classList.contains("front"));
  assert.deepEqual([...f.doc.querySelectorAll("iframe")].map(item => item.id).sort(), ["frame-a", "frame-b"]);
  f.dom.window.close();
});

test("the skeleton sweeps only while a render is on its way", () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  // Without an organization there is nothing to render with: the skeleton stands still until one is chosen.
  f.send({ type: "context", connected: true, choices: [{ id: "acme", name: "Acme Co" }, { id: "client-org", name: "Client" }] });
  assert.ok(!f.el("skeleton").classList.contains("loading"));
  f.send({ type: "context", connected: true, organization: { id: "acme", name: "Acme Co" }, choices: [] });
  assert.ok(f.el("skeleton").classList.contains("loading"));
  f.send({ type: "context", connected: false, organization: { id: "acme", name: "Acme Co" }, choices: [] });
  assert.equal(f.visible("skeleton"), true);
  assert.ok(!f.el("skeleton").classList.contains("loading"), "offline, the skeleton stands still");
  // A custom email offline shows its plain text instead.
  f.send({ type: "document", version: 2, email: { subject: "Thanks", textBody: "Thanks for subscribing.", htmlBody: "<p>Thanks</p>" } });
  assert.equal(f.visible("plain"), true);
  assert.equal(f.el("plain").textContent, "Thanks for subscribing.");
  assert.equal(f.visible("skeleton"), false);
  f.dom.window.close();
});

test("typing sends one edit after a pause or on leaving the field", () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  f.select("headline");
  const words = f.el("field-text") as HTMLTextAreaElement;
  words.focus();
  for (const value of ["W", "We", "Welcome"]) { f.type(words, value); f.advance(100); }
  assert.equal(f.edits().length, 0, "nothing is sent while typing goes on");
  words.blur();
  f.advance(1000);
  assert.deepEqual(f.edits(), [{ kind: "field", block: "headline", field: "text", value: "Welcome" }]);
  // A pause sends too, once.
  words.focus();
  f.type(words, "Welcome back");
  f.advance(249);
  assert.equal(f.edits().length, 1);
  f.advance(1);
  assert.deepEqual(f.edits().at(-1), { kind: "field", block: "headline", field: "text", value: "Welcome back" });
  words.blur();
  f.advance(1000);
  assert.equal(f.edits().length, 2, "leaving after the pause sends nothing more");
  // Ctrl or Cmd+Enter sends at once.
  f.type(words, "Welcome home");
  f.key(words, "Enter", { metaKey: true });
  assert.deepEqual(f.edits().at(-1), { kind: "field", block: "headline", field: "text", value: "Welcome home" });
  // Leaving without a change sends nothing.
  f.el("subject").focus(); f.el("subject").blur();
  assert.equal(f.edits().length, 3);
  f.dom.window.close();
});

test("a document update does not overwrite the field being typed in", () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  const subject = f.el("subject") as HTMLInputElement;
  assert.equal(subject.value, "Hello");
  subject.focus();
  f.type(subject, "Typing");
  subject.setSelectionRange(3, 3);
  const updated = clone(article);
  updated.subject = "Old";
  updated.blocks[0]!.text = "Changed in Source";
  f.send({ type: "document", version: 2, email: updated });
  assert.equal(subject.value, "Typing");
  assert.equal(subject.selectionStart, 3, "the caret stays where it was");
  assert.equal(f.doc.activeElement, subject);
  assert.equal(f.row("headline").textContent, "Heading · Changed in Source", "the outline updates");
  // Leaving sends the typed text; a copy of the document from before the edit does not undo it on screen.
  subject.blur();
  assert.deepEqual(f.edits().at(-1), { kind: "subject", value: "Typing" });
  f.send({ type: "document", version: 2, email: updated });
  assert.equal(subject.value, "Typing");
  const applied = clone(updated); applied.subject = "Typing";
  f.send({ type: "document", version: 3, email: applied });
  assert.equal(subject.value, "Typing");
  // A later change from Source reaches the field once nobody is typing in it.
  applied.subject = "From Source";
  f.send({ type: "document", version: 4, email: applied });
  assert.equal(subject.value, "From Source");
  // A preheader set to null is no preheader.
  f.send({ type: "document", version: 5, email: { ...applied, preheader: null } });
  assert.equal((f.el("preheader") as HTMLInputElement).value, "");
  f.dom.window.close();
});

test("move, add and remove are offered only where legal", () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  f.select("headline");
  assert.equal(f.el("move-up").hidden, true);
  assert.equal(f.el("move-down").hidden, false);
  f.select("cta");
  assert.equal(f.el("move-up").hidden, false);
  assert.equal(f.el("move-down").hidden, true);
  f.select("intro");
  assert.equal(f.el("move-up").hidden, true, "moves stay within the section");
  assert.equal(f.el("move-down").hidden, true);
  f.el("add-after").click();
  assert.equal(f.el("add-menu").hidden, false);
  assert.equal(f.el("add-after").getAttribute("aria-expanded"), "true");
  const items = [...f.el("add-menu").querySelectorAll<HTMLButtonElement>('[role="menuitem"]')];
  assert.deepEqual(items.map(item => item.textContent), ["Heading", "Text", "Highlight", "Button", "Picture", "Divider", "Space"]);
  assert.equal(f.doc.activeElement, items[0], "the menu takes focus");
  f.key(items[0]!, "ArrowDown");
  assert.equal(f.doc.activeElement, items[1]);
  items[1]!.click();
  assert.deepEqual(f.edits().at(-1), { kind: "add", after: "intro", type: "text" });
  assert.equal(f.el("add-menu").hidden, true);
  const added = clone(article);
  added.blocks[1]!.blocks!.push({ id: "text-1", type: "text", text: "" });
  f.send({ type: "document", version: 2, email: added });
  assert.equal(f.row("intro").getAttribute("aria-selected"), "true", "the selection waits for the host");
  f.send({ type: "select", block: "text-1" });
  assert.equal(f.row("text-1").getAttribute("aria-selected"), "true");
  assert.equal(f.el("block-title").textContent, "Text");
  assert.equal(f.doc.activeElement, f.el("field-text"), "a new block is ready for its words");
  f.el("remove-block").click();
  assert.deepEqual(f.edits().at(-1), { kind: "remove", block: "text-1" });
  f.select("cta");
  f.el("move-up").click();
  assert.deepEqual(f.edits().at(-1), { kind: "move", block: "cta", by: -1 });
  // Escape closes the menu and gives focus back to its button.
  f.el("add-after").click();
  f.key(f.doc.activeElement!, "Escape");
  assert.equal(f.el("add-menu").hidden, true);
  assert.equal(f.doc.activeElement, f.el("add-after"));
  f.dom.window.close();
});

test("the Add menu is brought into the inspector's view as it opens, and its first item takes the focus without a jump", () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  f.select("cta");
  const menu = f.el("add-menu");
  // jsdom cannot scroll; the page asks the menu to come into view, as it shows, the least distance.
  const scrolled: unknown[] = [];
  Object.assign(menu, { scrollIntoView: (options: unknown) => scrolled.push({ shown: !menu.hidden, options }) });
  const first = menu.querySelector<HTMLElement>('[role="menuitem"]')!;
  const focusedWith: unknown[] = [];
  const focus = first.focus.bind(first);
  Object.assign(first, { focus: (options?: FocusOptions) => { focusedWith.push(options); focus(options); } });
  f.el("add-after").click();
  // The options come from the page's realm: JSON brings them into this one.
  assert.deepEqual(clone(scrolled), [{ shown: true, options: { block: "nearest" } }]);
  assert.deepEqual(clone(focusedWith), [{ preventScroll: true }], "the focus does not scroll the menu back out of view");
  assert.equal(f.doc.activeElement, first);
  f.dom.window.close();
});

test("the selected block's ring shows on any email, and goes around a block too thin to hold it", () => {
  const f = fixture();
  f.doc.documentElement.style.setProperty("--vscode-focusBorder", "#0078d4");
  f.send({ type: "document", version: 1, email: article });
  f.send({ type: "render", seq: 1, html: rendered, text: "Hi", subject: "Hello", issues: [] });
  const frame = f.loadPreview();
  const css = [...frame.contentDocument!.querySelectorAll("style")].map(style => style.textContent).join("\n");
  // The focus colour with a white line inside it: on white the colour shows, on a brand colour close to it or a dark email the white does.
  assert.match(css, /\[data-block\]\[data-selected\]\{outline:2px solid #0078d4!important;outline-offset:-2px;box-shadow:inset 0 0 0 4px #fff!important\}/);
  assert.match(css, /\[data-block\]\[data-selected=around\]\{outline-offset:2px;box-shadow:none!important\}/);
  // The hover's dashed outline is for the other blocks: under the pointer the selected one keeps its ring.
  assert.match(css, /\[data-block\]:not\(\[data-selected\]\):hover:not\(:has\(\[data-block\]:hover\)\)\{outline:1px dashed #0078d4!important;outline-offset:-1px\}/);
  assert.equal(css.match(/:hover/g)?.length, 2, "no other hover rule can reach the selected block");
  const block = (id: string) => frame.contentDocument!.querySelector(`[data-block="${id}"]`)!;
  const tall = (id: string, height: number) => Object.assign(block(id), { getBoundingClientRect: () => ({ height }) });
  tall("headline", 40);
  tall("cta", 23);
  f.select("headline");
  assert.equal(block("headline").getAttribute("data-selected"), "", "a block the ring fits in is ringed inside its edge");
  f.select("cta");
  assert.equal(block("cta").getAttribute("data-selected"), "around", "one thinner than 24 pixels, a divider say, is ringed around it");
  assert.equal(block("headline").hasAttribute("data-selected"), false);
  f.dom.window.close();
});

test("the inspector is one quiet column: one mark on the selected row, no word said twice, room for the focus outline", () => {
  const html = emailPage({ title: "Welcome.email.json", script: "view.js", nonce: "abc123" });
  // The row's background marks it; the ring in the preview says which block it is. Its rule is as specific as the
  // hover's (one class, two attributes against one class, an attribute and :hover) and comes after it, so the row stays
  // marked under the pointer; a high contrast theme, which has no selection background, outlines it instead, and the
  // outline gives way to the focus ring.
  const hover = html.indexOf(".outline [role=treeitem]:hover{"), chosen = html.indexOf(".outline [role=treeitem][aria-selected=true]{");
  assert.ok(hover > 0 && chosen > hover, "the selected row's rule comes after the hover's");
  assert.equal(/\.outline \[role=treeitem\]\[aria-selected=true\]\{([^}]*)\}/.exec(html)?.[1], "background:var(--vscode-list-inactiveSelectionBackground)");
  assert.equal(/\.outline \[role=treeitem\]\[aria-selected=true\]:not\(:focus-visible\)\{([^}]*)\}/.exec(html)?.[1], "outline:1px solid var(--vscode-contrastActiveBorder,transparent);outline-offset:-1px");
  // A picture's block title already says Picture: the row's own name is for a screen reader alone.
  const doc = new JSDOM(html).window.document;
  assert.equal(doc.getElementById("field-src-label")?.className, "sr-only");
  assert.equal(doc.getElementById("field-src")?.getAttribute("aria-labelledby"), "field-src-label");
  // A control the keyboard reaches near the inspector's edge scrolls in with its outline in view.
  assert.match(html, /\.inspector\{[^}]*scroll-padding:8px 0[;}]/);
});

test("removing the selected block moves the selection, and the focus, to its neighbour", () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  f.select("body");
  f.el("remove-block").focus();
  f.el("remove-block").click();
  const removed = clone(article);
  removed.blocks.splice(1, 1);
  f.send({ type: "document", version: 2, email: removed });
  assert.equal(f.doc.querySelector('[aria-selected="true"]')?.getAttribute("data-block"), "cta", "the block that now stands where it was");
  assert.equal(f.doc.activeElement, f.row("cta"));
  assert.deepEqual(f.messages.at(-1), { type: "shown", version: 2, subject: "Hello", state: "", blocks: 2 });
  assert.equal(f.messages.at(-2).state.selected, "cta");
  f.dom.window.close();
});

test("issues mark their blocks and select them when clicked", () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  assert.equal(f.visible("issues"), false, "no issues, no group");
  f.send({ type: "render", seq: 1, html: rendered, text: "Hi", subject: "Hello", issues: [
    { code: "link_missing", block: "cta", message: "Add a link to the button “Go”." },
    { code: "unsubscribe_missing", message: "Add an unsubscribe link ({{unsubscribeUrl}}) to the footer." },
  ] });
  assert.equal(f.visible("issues"), true);
  assert.ok(f.row("cta").querySelector(".dot"), "the block with an issue carries a dot");
  assert.equal(f.row("headline").querySelector(".dot"), null);
  const buttons = [...f.el("issues").querySelectorAll("button")];
  assert.deepEqual(buttons.map(button => button.textContent), ["Add a link to the button “Go”."]);
  assert.match(f.el("issues").textContent ?? "", /Add an unsubscribe link/, "an issue without a block is a plain line");
  buttons[0]!.click();
  assert.equal(f.row("cta").getAttribute("aria-selected"), "true");
  assert.equal(f.el("block-title").textContent, "Button");
  f.send({ type: "render", seq: 2, html: rendered, text: "Hi", subject: "Hello", issues: [] });
  assert.equal(f.visible("issues"), false);
  assert.equal(f.row("cta").querySelector(".dot"), null);
  f.dom.window.close();
});

test("the action bar shows the host's state and acts, with the one button last", () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  f.send({ type: "bar", state: "Draft", acts: ["saveLayout", "publish"], layoutName: "Welcome" });
  assert.equal(f.el("bar-state").textContent, "Draft");
  assert.equal(f.el("bar-state").getAttribute("role"), "status");
  assert.ok(f.el("bar").classList.contains("bar"));
  const acts = [...f.el("bar-acts").querySelectorAll("button")];
  assert.deepEqual(acts.map(act => act.textContent), ["Save as layout", "Publish"]);
  assert.deepEqual(acts.map(act => act.classList.contains("primary")), [false, true]);
  assert.ok(acts[0]!.classList.contains("text-act"));
  acts[1]!.click();
  acts[1]!.click();
  assert.deepEqual(f.messages.filter(m => m.type === "act"), [{ type: "act", act: "publish" }], "a double click acts once");
  f.send({ type: "bar", state: "Draft", acts: ["saveLayout", "publish"], layoutName: "Welcome" });
  f.el("bar-acts").querySelector("button")!.click();
  assert.equal(f.visible("bar-form"), true);
  assert.equal(f.visible("bar-acts"), false);
  const name = f.el("layout-name") as HTMLInputElement;
  assert.equal(name.value, "Welcome");
  assert.equal(f.doc.activeElement, name);
  f.type(name, "Monthly letter");
  f.key(name, "Enter");
  assert.deepEqual(f.messages.at(-1), { type: "saveLayout", name: "Monthly letter" });
  assert.equal(f.visible("bar-form"), false);
  assert.equal(f.visible("bar-acts"), true);
  f.send({ type: "bar", state: "Draft", acts: ["saveLayout", "publish"], layoutName: "Monthly letter" });
  const count = f.messages.length;
  f.el("bar-acts").querySelector("button")!.click();
  assert.equal((f.el("layout-name") as HTMLInputElement).value, "Monthly letter");
  f.key(f.el("layout-name"), "Escape");
  assert.equal(f.visible("bar-form"), false);
  assert.equal(f.visible("bar-acts"), true);
  assert.equal(f.doc.activeElement, f.el("bar-acts").querySelector("button"), "focus goes back to the act");
  assert.equal(f.messages.length, count, "Escape sends nothing");
  // A blank name is refused in place.
  f.el("bar-acts").querySelector("button")!.click();
  f.type("layout-name", "   ");
  f.key(f.el("layout-name"), "Enter");
  assert.equal(f.messages.length, count);
  assert.equal(f.el("bar-notice").textContent, EMAIL_COPY.host.layoutNameInvalid);
  f.el("layout-cancel").click();
  assert.equal(f.visible("bar-form"), false);
  // A busy state shows the spinner and no acts; acts arrive in the host's order with their own styles.
  f.send({ type: "bar", state: C.publishing, acts: [] });
  assert.ok(f.el("bar-state").classList.contains("busy"));
  assert.equal(f.el("bar-acts").children.length, 0);
  f.send({ type: "bar", state: "Not connected to acme.example", acts: ["connect"] });
  assert.ok(!f.el("bar-state").classList.contains("busy"));
  assert.deepEqual([...f.el("bar-acts").querySelectorAll("button")].map(act => [act.textContent, act.className]), [["Connect", "primary"]]);
  f.dom.window.close();
});

test("a failed act shows one notice above the bar until the next act", () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  f.send({ type: "bar", state: "Draft", acts: ["saveLayout", "publish"] });
  f.send({ type: "problem", problem: { message: "Couldn’t publish the email. Try again.", reference: "tools-4f7c2a" } });
  const notice = f.el("bar-notice");
  assert.equal(f.visible("bar-notice"), true);
  assert.equal(notice.getAttribute("role"), "alert");
  assert.ok(notice.classList.contains("notice"));
  assert.match(notice.textContent ?? "", /Couldn’t publish the email/);
  assert.equal(notice.querySelector("summary")?.textContent, "Show details");
  assert.equal(notice.querySelector("code")?.textContent, "tools-4f7c2a");
  notice.querySelector<HTMLButtonElement>("details button")!.click();
  assert.deepEqual(f.messages.at(-1), { type: "copyProblemReference", reference: "tools-4f7c2a" });
  f.el("bar-acts").querySelectorAll("button")[1]!.click();
  assert.equal(f.visible("bar-notice"), false);
  assert.equal(notice.textContent, "");
  f.dom.window.close();
});

test("custom-mode emails show the rendered email without an outline or theme", () => {
  const f = fixture();
  const custom = { subject: "Thanks", preheader: "One more step", textBody: "Thanks for subscribing.", htmlBody: "<p>Thanks for subscribing.</p>" };
  f.send({ type: "document", version: 1, email: custom });
  assert.equal(f.visible("outline"), false);
  assert.equal(f.visible("theme"), false);
  assert.equal(f.visible("block"), false);
  assert.equal(f.visible("custom-note"), true);
  assert.match(f.el("custom-note").textContent ?? "", /Written in HTML/);
  assert.equal((f.el("subject") as HTMLInputElement).value, "Thanks");
  assert.equal((f.el("preheader") as HTMLInputElement).value, "One more step");
  f.send({ type: "render", seq: 1, html: "<html><body><p>Thanks for subscribing.</p></body></html>", text: "Thanks for subscribing.", subject: "Thanks", issues: [] });
  const frame = f.loadPreview();
  assert.match(frame.contentDocument!.body.textContent ?? "", /Thanks for subscribing/);
  assert.deepEqual(["width-desktop", "width-mobile", "width-plain"].map(id => f.visible(id)), [true, true, true]);
  const editHtml = [...f.el("custom-note").querySelectorAll("button")].find(button => button.textContent === "Edit HTML")!;
  editHtml.click();
  assert.deepEqual(f.messages.at(-1), { type: "mode", mode: "split" });
  // With no HTML at all, plain text is the only view.
  f.send({ type: "document", version: 2, email: { ...custom, htmlBody: "" } });
  assert.deepEqual(["width-desktop", "width-mobile", "width-plain"].map(id => f.visible(id)), [false, false, true]);
  assert.equal(f.visible("plain"), true);
  assert.match(f.el("custom-note").textContent ?? "", /Plain text only/);
  f.el("view-source").click();
  assert.deepEqual(f.messages.at(-1), { type: "mode", mode: "source" });
  f.dom.window.close();
});

test("Preview with lists only the merge tags used and keeps the values in the page state", async () => {
  const f = fixture();
  const tagged = clone(article);
  tagged.blocks[2]!.label = "Hi {{fields.firstName}}";
  f.send({ type: "document", version: 1, email: tagged });
  assert.equal(f.visible("samples"), true);
  const inputs = [...f.el("samples").querySelectorAll<HTMLInputElement>("input")];
  assert.deepEqual(inputs.map(input => input.closest("label")?.textContent), ["Name", "fields.firstName"]);
  assert.deepEqual(inputs.map(input => input.placeholder), ["Sample value", "Sample value"]);
  f.el("samples").querySelector("summary")!.click();
  await settle();
  assert.equal(f.messages.at(-1).state.samplesOpen, true);
  f.type(inputs[0]!, "Ada");
  assert.equal(f.messages.at(-1).type, "viewState");
  assert.equal(f.messages.at(-1).state.samples, undefined, "values wait for the pause");
  f.advance(250);
  assert.deepEqual(f.messages.at(-1), { type: "viewState", state: { width: "desktop", samplesOpen: true, samples: { displayName: "Ada" } } });
  assert.deepEqual((f.state() as any).samples, { displayName: "Ada" });
  assert.equal(f.edits().length, 0, "sample values never touch the email");
  // A later document keeps the inputs and what is typed in them.
  inputs[1]!.focus();
  f.type(inputs[1]!, "Grace");
  f.send({ type: "document", version: 2, email: tagged });
  assert.equal(f.el("samples").querySelectorAll("input")[1], inputs[1]);
  assert.equal(inputs[1]!.value, "Grace");
  f.send({ type: "document", version: 3, email: { ...clone(article), blocks: [{ id: "headline", type: "heading", text: "Hi" }] } });
  assert.equal(f.visible("samples"), false, "no merge tags, no disclosure");
  // Restored values fill the inputs.
  const g = fixture({ width: "desktop", samples: { displayName: "Lin" } });
  g.send({ type: "document", version: 1, email: article });
  assert.equal(g.el("samples").querySelector("input")!.value, "Lin");
  f.dom.window.close(); g.dom.window.close();
});

test("the width choice switches the preview and is restored", async () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  f.send({ type: "render", seq: 1, html: rendered, text: "Hi\n\nHello Ada", subject: "Hello", issues: [] });
  f.loadPreview();
  assert.equal(f.el("width-desktop").getAttribute("aria-checked"), "true");
  f.el("width-mobile").click();
  assert.ok(f.el("preview").classList.contains("mobile"));
  assert.equal(f.el("width-mobile").getAttribute("aria-checked"), "true");
  assert.equal(f.el("width-desktop").getAttribute("aria-checked"), "false");
  assert.deepEqual(f.messages.at(-1), { type: "viewState", state: { width: "mobile" } });
  f.send({ type: "restore", state: { width: "plain", themeOpen: true } });
  assert.ok(!f.el("preview").classList.contains("mobile"));
  assert.equal(f.visible("plain"), true);
  assert.equal(f.el("plain").localName, "pre");
  assert.equal(f.el("plain").textContent, "Hi\n\nHello Ada");
  assert.equal((f.el("theme") as HTMLDetailsElement).open, true);
  await settle();
  assert.deepEqual(f.messages.at(-1), { type: "viewState", state: { width: "mobile" } }, "a restore is not echoed back");
  // Arrow keys move the choice within the group.
  f.key(f.el("width-plain"), "ArrowLeft");
  assert.equal(f.el("width-mobile").getAttribute("aria-checked"), "true");
  assert.equal(f.doc.activeElement, f.el("width-mobile"));
  // The preview's own state classes never style what it holds (a "plain" stage once took the text's column and font).
  const held = new Set([...f.el("preview").querySelectorAll("[class]")].flatMap(element => [...element.classList]));
  for (const width of ["width-desktop", "width-mobile", "width-plain"]) {
    f.el(width).click();
    assert.deepEqual([...f.el("preview").classList].filter(name => held.has(name)), [], width);
  }
  // The webview's own saved state is read on load.
  const g = fixture({ width: "mobile", selected: "cta" });
  g.send({ type: "document", version: 1, email: article });
  assert.ok(g.el("preview").classList.contains("mobile"));
  assert.equal(g.row("cta").getAttribute("aria-selected"), "true");
  f.dom.window.close(); g.dom.window.close();
});

test("a render failure keeps the last preview dimmed with the problem", () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  f.send({ type: "render", seq: 1, html: rendered, text: "Hi", subject: "Hello", issues: [] });
  const frame = f.loadPreview();
  const budget = "This email has too much formatted text. Shorten it or split it into more emails.";
  f.send({ type: "renderFailed", seq: 2, problem: { message: budget, reference: "tools-77aa01" } });
  assert.ok(f.el("preview").classList.contains("stale"));
  assert.ok(frame.classList.contains("front"), "the last good preview stays");
  assert.equal(f.visible("preview-notice"), true);
  assert.equal(f.el("preview-notice").getAttribute("role"), "alert");
  assert.equal(f.el("preview-notice").firstChild?.textContent, budget, "the engine's sentence, verbatim");
  assert.equal(f.el("preview-notice").querySelector("summary")?.textContent, "Show details");
  assert.equal(f.el("preview-notice").querySelector("code")?.textContent, "tools-77aa01");
  // An older answer never replaces a newer one.
  f.send({ type: "render", seq: 1, html: rendered, text: "Hi", subject: "Hello", issues: [] });
  assert.equal(f.doc.querySelector('iframe[data-pending="1"]'), null);
  f.send({ type: "render", seq: 3, html: rendered, text: "Hi", subject: "Hello", issues: [] });
  f.loadPreview();
  assert.ok(!f.el("preview").classList.contains("stale"));
  assert.equal(f.visible("preview-notice"), false);
  // A render that was still loading when a later one failed shows, but the failure stays with it.
  f.send({ type: "render", seq: 4, html: rendered, text: "Hi", subject: "Hello", issues: [] });
  f.send({ type: "renderFailed", seq: 5, problem: { message: budget } });
  f.loadPreview();
  assert.ok(f.el("preview").classList.contains("stale"));
  assert.equal(f.el("preview-notice").textContent, budget);
  f.send({ type: "render", seq: 6, html: rendered, text: "Hi", subject: "Hello", issues: [] });
  f.loadPreview();
  assert.ok(!f.el("preview").classList.contains("stale"));
  // An email that cannot be read keeps its last preview, dimmed, with the reason, whatever finishes loading meanwhile.
  f.send({ type: "render", seq: 7, html: rendered, text: "Hi", subject: "Hello", issues: [] });
  f.send({ type: "document", version: 2, email: null, invalid: C.invalidJson });
  f.loadPreview();
  assert.ok(f.el("preview").classList.contains("stale"));
  assert.equal(f.el("preview-notice").textContent, C.invalidJson);
  assert.equal(f.visible("outline"), false, "nothing to edit while the email can’t be read");
  assert.equal(f.visible("subject"), false);
  f.send({ type: "document", version: 3, email: article });
  assert.equal(f.visible("outline"), true);
  assert.equal(f.visible("preview-notice"), false);
  f.dom.window.close();
});

test("messages from another origin are ignored", () => {
  const f = fixture();
  const forged = (data: unknown) => f.dom.window.dispatchEvent(new f.dom.window.MessageEvent("message", { data, origin: "https://attacker.example" }));
  forged({ type: "document", version: 1, email: article });
  forged({ type: "bar", state: "Published to Acme", acts: ["publish"] });
  forged({ type: "select", block: "cta" });
  assert.equal(f.doc.querySelectorAll('[role="treeitem"]').length, 0);
  assert.equal(f.el("bar-state").textContent, "");
  assert.deepEqual(f.messages.map(m => m.type), ["ready"]);
  f.send({ type: "document", version: 1, email: article });
  assert.equal(f.doc.querySelectorAll('[role="treeitem"]').length, 4);
  assert.equal(f.doc.querySelector('[aria-selected="true"]'), null, "the forged selection never applied");
  f.dom.window.close();
});

test("arrow keys move through the outline and Alt+Arrow moves a block", () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  f.select("headline");
  f.row("headline").focus();
  assert.equal(f.row("headline").tabIndex, 0);
  assert.equal(f.row("body").tabIndex, -1, "one stop in the outline");
  f.key(f.row("headline"), "ArrowDown");
  assert.equal(f.doc.activeElement, f.row("body"));
  assert.equal(f.row("body").getAttribute("aria-selected"), "true");
  f.key(f.row("body"), "ArrowDown");
  assert.equal(f.doc.activeElement, f.row("intro"));
  f.key(f.row("intro"), "ArrowUp");
  assert.equal(f.doc.activeElement, f.row("body"));
  f.key(f.row("body"), "End");
  assert.equal(f.doc.activeElement, f.row("cta"));
  f.key(f.row("cta"), "Home");
  assert.equal(f.doc.activeElement, f.row("headline"));
  const before = f.edits().length;
  f.key(f.row("headline"), "ArrowUp", { altKey: true });
  assert.equal(f.edits().length, before, "the first block cannot move up");
  f.key(f.row("headline"), "ArrowDown", { altKey: true });
  assert.deepEqual(f.edits().at(-1), { kind: "move", block: "headline", by: 1 });
  // The moved block keeps the selection and the focus when the document comes back.
  const moved = clone(article);
  moved.blocks = [moved.blocks[1]!, moved.blocks[0]!, moved.blocks[2]!];
  f.send({ type: "document", version: 2, email: moved });
  assert.equal(f.doc.activeElement, f.row("headline"));
  assert.equal(f.row("headline").getAttribute("aria-selected"), "true");
  assert.deepEqual([...f.doc.querySelectorAll('[role="treeitem"]')].map(item => item.getAttribute("data-block")), ["body", "intro", "headline", "cta"]);
  f.key(f.row("headline"), "Enter");
  assert.equal(f.doc.activeElement, f.el("field-text"), "Enter goes to the first field");
  f.dom.window.close();
});

test("theme colours and fonts send theme edits", async () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  const theme = f.el("theme") as HTMLDetailsElement;
  assert.equal(theme.querySelector("summary")?.textContent, "Theme");
  const colours = [...theme.querySelectorAll<HTMLInputElement>('input[type="color"]')];
  assert.deepEqual(colours.map(input => input.closest("label")?.textContent), ["Page", "Canvas", "Primary", "Secondary", "Accent", "Tint", "Text", "Muted"]);
  assert.equal((f.el("theme-primary") as HTMLInputElement).value, "#2f5d8a");
  theme.querySelector("summary")!.click();
  await settle();
  assert.equal(f.messages.at(-1).state.themeOpen, true);
  f.type("theme-primary", "#123456");
  f.advance(250);
  assert.deepEqual(f.edits().at(-1), { kind: "theme", key: "primary", value: "#123456" });
  const font = f.el("theme-headingFont") as HTMLInputElement;
  assert.equal(font.value, "Georgia");
  font.focus();
  f.type(font, "Bad<Font");
  font.blur();
  assert.equal(f.el("theme-headingFont-notice").textContent, "Enter a font name using letters, digits, spaces or hyphens.");
  assert.equal(f.edits().length, 1);
  font.focus();
  f.type(font, "Georgia Pro");
  font.blur();
  assert.deepEqual(f.edits().at(-1), { kind: "theme", key: "headingFont", value: "Georgia Pro" });
  assert.equal(f.el("theme-headingFont-notice").textContent, "");
  f.dom.window.close();
});

test("picture acts post the block and the action, and Remove is absent without a picture", () => {
  const f = fixture();
  const pictured = { ...clone(article), blocks: [{ id: "logo", type: "image", src: "", alt: "Acme logo", width: 200 }, ...clone(article).blocks] };
  f.send({ type: "document", version: 1, email: pictured });
  f.select("logo");
  assert.equal(f.el("block-title").textContent, "Picture");
  assert.equal(f.visible("picture-none"), true);
  assert.equal(f.el("picture-none").textContent, "No picture");
  assert.equal(f.visible("picture-remove"), false);
  f.el("picture-choose").click();
  assert.deepEqual(f.messages.at(-1), { type: "picture", block: "logo", action: "choose" });
  f.el("picture-upload").click();
  assert.deepEqual(f.messages.at(-1), { type: "picture", block: "logo", action: "upload" });
  assert.equal((f.el("field-alt") as HTMLInputElement).value, "Acme logo");
  pictured.blocks[0]!.src = "artifact:logo-1";
  f.send({ type: "document", version: 2, email: pictured });
  assert.equal(f.visible("picture-none"), false);
  assert.equal(f.visible("picture-remove"), true);
  f.el("picture-remove").click();
  assert.deepEqual(f.messages.at(-1), { type: "picture", block: "logo", action: "remove" });
  assert.deepEqual(["picture-choose", "picture-upload", "picture-remove"].map(id => f.el(id).textContent), ["Choose from Files", "Upload", "Remove"]);
  assert.equal(f.edits().length, 0, "the host sets the picture");
  f.dom.window.close();
});

test("every document message reports what was shown", () => {
  const f = fixture();
  f.send({ type: "bar", state: "Draft", acts: ["saveLayout", "publish"] });
  f.send({ type: "document", version: 3, email: article });
  assert.deepEqual(f.messages.at(-1), { type: "shown", version: 3, subject: "Hello", state: "Draft", blocks: 4 });
  f.send({ type: "bar", state: "Not connected", acts: ["connect"] });
  f.send({ type: "document", version: 4, email: { subject: "Thanks for subscribing", textBody: "Thanks.", htmlBody: "<p>Thanks.</p>" } });
  assert.deepEqual(f.messages.at(-1), { type: "shown", version: 4, subject: "Thanks for subscribing", state: "Not connected", blocks: 0 });
  f.send({ type: "document", version: 5, email: null, invalid: C.invalidJson });
  assert.deepEqual(f.messages.at(-1), { type: "shown", version: 5, subject: "", state: "Not connected", blocks: 0 });
  // Until the person acts, the page sends only that it is ready and what it showed.
  assert.deepEqual([...new Set(f.messages.map(m => m.type))], ["ready", "shown"]);
  f.dom.window.close();
});

test("each field refuses what the engine would refuse, in place, before anything is sent", () => {
  const f = fixture();
  const full = { ...clone(article), blocks: [
    ...clone(article).blocks,
    { id: "follow", type: "social", title: "Follow us", links: [{ network: "Mail", href: "mailto:hello@acme.example", icon: "artifact:mail-icon" }] },
    { id: "foot", type: "footer", lines: ["Acme Co, 1 Main Street"], links: [{ label: "Unsubscribe", href: "{{unsubscribeUrl}}" }] },
  ] };
  f.send({ type: "document", version: 1, email: full });
  const refuses = (id: string, value: string, sentence: string) => {
    const before = f.edits().length;
    const field = f.el(id) as HTMLInputElement;
    field.focus(); f.type(field, value); field.blur();
    assert.equal(f.el(`${id}-notice`).textContent, sentence, `${id}: ${JSON.stringify(value).slice(0, 40)}`);
    assert.equal(f.edits().length, before, `${id}: nothing was sent`);
  };
  const accepts = (id: string, value: string) => {
    const before = f.edits().length;
    const field = f.el(id) as HTMLInputElement;
    field.focus(); f.type(field, value); field.blur();
    assert.equal(f.el(`${id}-notice`).textContent, "", id);
    assert.equal(f.edits().length, before + 1, `${id}: sent`);
  };

  f.select("cta");
  // A pause while typing never scolds; leaving the field does, and a pause clears it once the value is right.
  const label = f.el("field-label") as HTMLInputElement;
  label.focus(); f.type(label, ""); f.advance(250);
  assert.equal(f.el("field-label-notice").textContent, "");
  label.blur();
  assert.equal(f.el("field-label-notice").textContent, C.buttonLabelEmpty);
  // The refusal is the field's own: a screen reader hears it with the field.
  assert.equal(label.getAttribute("aria-invalid"), "true");
  assert.equal(label.getAttribute("aria-describedby"), "field-label-notice");
  label.focus(); f.type(label, "Go now"); f.advance(250);
  assert.equal(f.el("field-label-notice").textContent, "");
  assert.equal(label.hasAttribute("aria-invalid"), false);
  assert.deepEqual(f.edits().at(-1), { kind: "field", block: "cta", field: "label", value: "Go now" });
  label.blur();
  refuses("field-label", "", C.buttonLabelEmpty);
  refuses("field-label", "é".repeat(101), C.buttonLabelInvalid);
  accepts("field-label", "é".repeat(100));
  refuses("field-href", "javascript:alert(1)", "Enter a web address, an email address (mailto:), a phone number (tel:) or a link tag such as {{unsubscribeUrl}}.");
  refuses("field-href", "https://{{email}}.example", "Enter a web address, an email address (mailto:), a phone number (tel:) or a link tag such as {{unsubscribeUrl}}.");
  accepts("field-href", "https://example.com/?e={{email}}");

  f.select("intro");
  refuses("field-text", "See [our site](ftp://acme.example)", C.wordsLinkAddress);
  refuses("field-text", "See [our site]()", C.wordsLinkAddress);
  refuses("field-text", "See [ ](https://acme.example)", C.wordsLinkLabel);
  accepts("field-text", "See [our site](https://acme.example/a_(b)) and [unsubscribe]({{unsubscribeUrl}})");
  accepts("field-text", "Not a link: [a] (b), \\[c\\](d) and [e](f");
  // Links never span list items, but a paragraph's lines are one text: Go reads them the same way.
  accepts("field-text", "- [a](https://acme.example\n- b)");
  refuses("field-text", "[a](https://acme.example\n  b)", C.wordsLinkAddress);

  f.select("foot");
  refuses("field-lines", Array.from({ length: 13 }, (_, n) => `Line ${n + 1}`).join("\n"), C.tooManyLines);
  refuses("field-lines", "x".repeat(301), C.lineTooLong);
  accepts("field-lines", ["Acme Co, 1 Main Street", "é".repeat(300)].join("\n"));
  assert.deepEqual(f.edits().at(-1), { kind: "field", block: "foot", field: "lines", value: ["Acme Co, 1 Main Street", "é".repeat(300)] });
  const linkLabel = f.el("field-links").querySelector<HTMLInputElement>('input[data-part="name"]')!;
  linkLabel.focus(); f.type(linkLabel, ""); linkLabel.blur();
  assert.equal(f.el("field-links-notice").textContent, C.linkLabelInvalid);

  f.select("follow");
  const network = f.el("field-links").querySelector<HTMLInputElement>('input[data-part="name"]')!;
  assert.equal(network.value, "Mail");
  network.focus(); f.type(network, ""); network.blur();
  assert.equal(f.el("field-links-notice").textContent, C.networkEmpty);
  assert.equal(network.getAttribute("aria-invalid"), "true", "the link that is wrong says so");
  assert.equal(network.getAttribute("aria-describedby"), "field-links-notice");
  network.focus(); f.type(network, "x".repeat(41)); network.blur();
  assert.equal(f.el("field-links-notice").textContent, C.networkInvalid);
  const address = f.el("field-links").querySelector<HTMLInputElement>('input[data-part="href"]')!;
  network.focus(); f.type(network, "Email"); f.type(address, "ftp://acme.example"); network.blur();
  assert.equal(network.hasAttribute("aria-invalid"), false);
  assert.equal(address.getAttribute("aria-invalid"), "true");
  address.focus(); f.type(address, "mailto:hello@acme.example");
  network.focus(); f.type(network, "Email"); network.blur();
  assert.equal(f.el("field-links-notice").textContent, "");
  assert.deepEqual(f.edits().at(-1), { kind: "field", block: "follow", field: "links", value: [{ network: "Email", href: "mailto:hello@acme.example", icon: "artifact:mail-icon" }] });

  // The subject and the preheader follow the model's own rules.
  refuses("subject", "", "Enter a subject on one line.");
  accepts("subject", "Hello again");
  refuses("preheader", "x".repeat(201), "The preheader must be one line of 200 characters or fewer.");
  accepts("preheader", "Read this first");
  accepts("preheader", "");
  assert.deepEqual(f.edits().at(-1), { kind: "preheader", value: "" }, "clearing what was just sent is a change, though the host has not answered yet");
  f.dom.window.close();
});

test("link rows add, edit and remove links, within the limits", () => {
  const f = fixture();
  const footed = { ...clone(article), blocks: [{ id: "foot", type: "footer", lines: [], links: [{ label: "Unsubscribe", href: "{{unsubscribeUrl}}" }] }] };
  f.send({ type: "document", version: 1, email: footed });
  f.select("foot");
  assert.equal(f.el("field-links").querySelectorAll(".link-row").length, 1);
  f.el("links-add").click();
  const rows = f.el("field-links").querySelectorAll(".link-row");
  assert.equal(rows.length, 2);
  const [name, href] = [...rows[1]!.querySelectorAll<HTMLInputElement>("input")];
  assert.equal(f.doc.activeElement, name, "a new link is ready to name");
  f.type(name!, "Preferences");
  href!.focus();
  assert.deepEqual(f.edits().at(-1), { kind: "field", block: "foot", field: "links", value: [{ label: "Unsubscribe", href: "{{unsubscribeUrl}}" }, { label: "Preferences" }] });
  f.type(href!, "{{preferencesUrl}}");
  href!.blur();
  assert.deepEqual(f.edits().at(-1), { kind: "field", block: "foot", field: "links", value: [{ label: "Unsubscribe", href: "{{unsubscribeUrl}}" }, { label: "Preferences", href: "{{preferencesUrl}}" }] });
  rows[0]!.querySelector<HTMLButtonElement>(".link-remove")!.click();
  assert.deepEqual(f.edits().at(-1), { kind: "field", block: "foot", field: "links", value: [{ label: "Preferences", href: "{{preferencesUrl}}" }] });
  assert.equal(f.doc.activeElement, f.el("field-links").querySelector('input[data-part="name"]'), "focus stays among the links");
  // Eight links is the most a block takes; a social block keeps at least one.
  f.el("subject").focus();
  footed.blocks[0]!.links = Array.from({ length: 8 }, (_, n) => ({ label: `Link ${n + 1}` }));
  f.send({ type: "document", version: 2, email: footed });
  assert.equal(f.el("field-links").querySelectorAll(".link-row").length, 8);
  assert.equal(f.visible("links-add"), false);
  const social = { ...clone(article), blocks: [{ id: "follow", type: "social", links: [{ network: "Mail", href: "mailto:a@acme.example" }] }] };
  f.send({ type: "document", version: 3, email: social });
  f.select("follow");
  assert.equal(f.el("field-links").querySelector<HTMLElement>(".link-remove")!.hidden, true);
  assert.equal(f.el("field-links").querySelector(".link-row")?.querySelector("label")?.textContent, "Network");
  f.dom.window.close();
});

test("colours, alignment and the brand switch send at once", () => {
  const f = fixture();
  const coloured = clone(article);
  coloured.blocks[0] = { id: "headline", type: "heading", text: "Hi", color: "primary", align: "center" } as any;
  f.send({ type: "document", version: 1, email: coloured });
  f.select("headline");
  const colour = f.el("field-color") as HTMLSelectElement;
  assert.deepEqual([...colour.options].filter(option => option.value).map(option => option.textContent), ["Page", "Canvas", "Primary", "Secondary", "Accent", "Tint", "Text", "Muted", "White"]);
  assert.equal(colour.value, "primary");
  assert.equal(f.el("field-color-swatch").style.backgroundColor, "rgb(47, 93, 138)");
  colour.value = "accent";
  colour.dispatchEvent(new f.dom.window.Event("change", { bubbles: true }));
  assert.deepEqual(f.edits().at(-1), { kind: "field", block: "headline", field: "color", value: "accent" });
  assert.equal(f.el("field-color-swatch").style.backgroundColor, "rgb(217, 130, 43)");
  // Default is the layout's own colour: choosing it takes the block's away.
  assert.equal(colour.options[0]!.textContent, "Default");
  colour.value = "";
  colour.dispatchEvent(new f.dom.window.Event("change", { bubbles: true }));
  assert.deepEqual(f.edits().at(-1), { kind: "field", block: "headline", field: "color", value: "" });
  assert.ok(f.el("field-color-swatch").classList.contains("empty"), "no colour, no swatch");
  const align = [...f.el("field-align").querySelectorAll<HTMLElement>('[role="radio"]')];
  assert.deepEqual(align.map(option => [option.textContent, option.getAttribute("aria-checked")]), [["Left", "false"], ["Center", "true"], ["Right", "false"]]);
  align[2]!.click();
  assert.deepEqual(f.edits().at(-1), { kind: "field", block: "headline", field: "align", value: "right" });
  const brand = f.el("field-brand") as HTMLInputElement;
  assert.equal(brand.getAttribute("role"), "switch");
  assert.equal(brand.closest("label")?.textContent, "Same in every email");
  assert.equal(brand.checked, false);
  brand.click();
  assert.deepEqual(f.edits().at(-1), { kind: "role", block: "headline", brand: true });
  // A block inside a brand section is brand too, by inheritance.
  const branded = clone(article);
  (branded.blocks[1] as any).role = "brand";
  f.send({ type: "document", version: 2, email: branded });
  f.select("intro");
  assert.equal((f.el("field-brand") as HTMLInputElement).checked, true);
  // Each type shows its own fields, in order, and the switch last.
  f.select("cta");
  const shown = [...f.el("block").querySelectorAll<HTMLElement>("[data-field]")].filter(row => !row.hidden).map(row => row.dataset.field);
  assert.deepEqual(shown, [...EDITABLE_FIELDS.button, "brand"]);
  f.dom.window.close();
});

test("the page is the static editor: every id the view and the host use, and only the email copy", () => {
  const html = emailPage({ title: "Welcome.email.json", script: "https://file+.vscode-resource.test/out/emailView.js", nonce: "abc123" });
  const doc = new JSDOM(html).window.document;
  for (const id of ["view-design", "view-source", "width-desktop", "width-mobile", "width-plain", "preview", "skeleton", "plain", "preview-notice", "subject", "preheader", "organization-row", "organization", "issues", "outline", "block", "block-title", "move-up", "move-down", "add-after", "add-menu", "remove-block", "theme", "samples", "custom-note", "bar", "bar-state", "bar-acts", "bar-form", "layout-name", "layout-cancel", "layout-save", "bar-notice", "frame-a", "frame-b",
    "field-text", "field-label", "field-href", "field-alt", "field-src", "field-color", "field-background", "field-align", "field-height", "field-title", "field-links", "field-lines", "field-html", "field-brand"]) {
    assert.ok(doc.getElementById(id), id);
  }
  assert.equal(doc.querySelector('meta[http-equiv="Content-Security-Policy"]')?.getAttribute("content"), "default-src 'none'; script-src 'nonce-abc123'; style-src 'unsafe-inline'; img-src data:; frame-src 'self'; form-action 'none'; base-uri 'none'");
  assert.equal(doc.querySelector("script")?.getAttribute("src"), "https://file+.vscode-resource.test/out/emailView.js");
  for (const frame of doc.querySelectorAll("iframe")) {
    assert.equal(frame.getAttribute("sandbox"), "allow-same-origin", "frames never run scripts");
    assert.ok(frame.getAttribute("title"), "a frame is named for a screen reader");
  }
  assert.equal(doc.querySelector(".views")?.querySelectorAll("[aria-pressed]").length, 2);
  assert.equal(doc.getElementById("view-design")?.getAttribute("aria-pressed"), "true");
  assert.equal(doc.getElementById("width-desktop")?.closest('[role="radiogroup"]')?.getAttribute("aria-label"), C.previewWidth);
  assert.equal(doc.getElementById("view-design")?.closest('[role="group"]')?.getAttribute("aria-label"), C.emailView);
  // The theme lists its colours in the model's order.
  assert.deepEqual([...doc.querySelectorAll('#theme input[type="color"]')].map(input => input.id), THEME_COLOURS.map(key => `theme-${key}`));
  // Every word a person can see or hear on the page is from EMAIL_COPY.
  const copy = new Set<string>();
  const collect = (value: unknown): void => { if (typeof value === "string") copy.add(value); else if (value && typeof value === "object") Object.values(value).forEach(collect); };
  collect(EMAIL_COPY);
  const walker = doc.createTreeWalker(doc.body, 4);
  const words: string[] = [];
  for (let node = walker.nextNode(); node; node = walker.nextNode()) if (node.textContent!.trim() && node.parentElement?.localName !== "script") words.push(node.textContent!.trim());
  for (const element of doc.querySelectorAll("[aria-label],[title],[placeholder]")) for (const name of ["aria-label", "title", "placeholder"]) { const value = element.getAttribute(name); if (value) words.push(value); }
  assert.ok(words.length > 40);
  for (const word of words) assert.ok(copy.has(word), `"${word}" is not in EMAIL_COPY`);
  // Each type's fields keep their order in the one list of rows.
  const rows = [...doc.querySelectorAll<HTMLElement>("[data-field]")].map(row => row.dataset.field);
  for (const type of BLOCK_TYPES) assert.deepEqual(rows.filter(field => (EDITABLE_FIELDS[type] as readonly string[]).includes(field!)), [...EDITABLE_FIELDS[type]], type);
  assert.equal(rows.at(-1), "brand", "the switch comes last");
  // The block names the outline shows are the copy's.
  assert.deepEqual({ ...BLOCK_NAMES }, { ...C.blockNames });
  assert.deepEqual(Object.keys(C.blockNames).sort(), [...BLOCK_TYPES].sort());
});

test("the organization is asked only when the host offers choices", () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  assert.equal(f.visible("organization-row"), false);
  const choices = [{ id: "acme", name: "Acme Co" }, { id: "client-org", name: "Client" }];
  f.send({ type: "context", connected: true, choices });
  assert.equal(f.visible("organization-row"), true);
  const select = f.el("organization") as HTMLSelectElement;
  assert.deepEqual([...select.options].map(option => [option.value, option.textContent]), [["", "Choose the organization"], ["acme", "Acme Co"], ["client-org", "Client"]]);
  assert.equal(select.value, "");
  select.value = "client-org";
  select.dispatchEvent(new f.dom.window.Event("change", { bubbles: true }));
  assert.deepEqual(f.messages.at(-1), { type: "organization", id: "client-org" });
  f.send({ type: "context", connected: true, organization: { id: "client-org", name: "Client" }, choices });
  assert.deepEqual([...select.options].map(option => option.value), ["acme", "client-org"], "once chosen, the prompt goes");
  assert.equal(select.value, "client-org");
  f.send({ type: "context", connected: true, organization: { id: "client-org", name: "Client" }, choices: [] });
  assert.equal(f.visible("organization-row"), false);
  f.dom.window.close();
});

test("the preview outlines blocks in the editor's focus colour, and nothing but a colour gets into its style", () => {
  const f = fixture();
  f.doc.documentElement.style.setProperty("--vscode-focusBorder", "#0078d4");
  f.send({ type: "document", version: 1, email: article });
  f.send({ type: "render", seq: 1, html: rendered, text: "Hi", subject: "Hello", issues: [] });
  const srcdoc = f.doc.querySelector('iframe[data-pending="1"]')!.getAttribute("srcdoc")!;
  const policy = srcdoc.indexOf('<meta http-equiv="Content-Security-Policy"');
  assert.ok(policy > 0 && policy < srcdoc.indexOf("<style"), "the policy comes first");
  const frame = f.loadPreview();
  const css = [...frame.contentDocument!.querySelectorAll("style")].map(style => style.textContent).join("\n");
  assert.match(css, /\[data-block\]\{cursor:pointer\}/);
  assert.match(css, /outline:1px dashed #0078d4/);
  assert.match(css, /outline:2px solid #0078d4/);
  f.doc.documentElement.style.setProperty("--vscode-focusBorder", "red}body{background:url(https://outside.test/x.png)}");
  f.send({ type: "render", seq: 2, html: rendered, text: "Hi", subject: "Hello", issues: [] });
  const next = f.loadPreview();
  const css2 = [...next.contentDocument!.querySelectorAll("style")].map(style => style.textContent).join("\n");
  assert.doesNotMatch(css2, /outside|background/);
  assert.match(css2, /outline:2px solid Highlight/);
  f.dom.window.close();
});

test("block ids from the file are only ever compared, never written into a selector", () => {
  const f = fixture();
  const odd = 'x"],body{color:red}[x="';
  const email = { ...clone(article), blocks: [{ id: odd, type: "heading", text: "Odd" }, ...clone(article).blocks] };
  f.send({ type: "document", version: 1, email });
  f.send({ type: "render", seq: 1, html: rendered, text: "Hi", subject: "Hello", issues: [{ code: "link_missing", block: odd, message: "Odd" }] });
  f.loadPreview();
  const oddRow = () => [...f.doc.querySelectorAll<HTMLElement>('[role="treeitem"]')].find(item => item.getAttribute("data-block") === odd)!;
  oddRow().click();
  assert.equal(oddRow().getAttribute("aria-selected"), "true");
  assert.equal(f.el("block-title").textContent, "Heading");
  f.send({ type: "select", block: "cta" });
  assert.equal(f.row("cta").getAttribute("aria-selected"), "true");
  f.send({ type: "select", block: odd });
  assert.equal(oddRow().getAttribute("aria-selected"), "true");
  f.select("cta");
  f.el("issues").querySelector("button")!.click();
  assert.equal(oddRow().getAttribute("aria-selected"), "true");
  oddRow().focus();
  f.key(oddRow(), "ArrowDown");
  assert.equal(f.row("headline").getAttribute("aria-selected"), "true");
  f.dom.window.close();
});

test("when the person works in another window, a field they left takes the document's value, and nothing is focused", () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  const subject = f.el("subject") as HTMLInputElement;
  subject.focus();
  f.type(subject, "Hello there");
  // Split view: the person clicks into the Source editor beside the page.
  f.away();
  assert.equal(f.doc.activeElement, subject, "the field is still the page's active element");
  assert.deepEqual(f.edits().at(-1), { kind: "subject", value: "Hello there" }, "leaving for Source sends the typing");
  const before = f.focuses.length;
  f.send({ type: "document", version: 2, email: { ...clone(article), subject: "Hello there" } });
  f.send({ type: "document", version: 3, email: { ...clone(article), subject: "Rewritten in Source" } });
  assert.equal(subject.value, "Rewritten in Source", "a keystroke on return cannot send stale text over Source's edit");
  assert.equal(f.focuses.length, before, "no element was focused");
  f.dom.window.close();
});

test("no host update takes focus while the editor's window is elsewhere", () => {
  const f = fixture();
  const tagged = clone(article);
  tagged.blocks[2]!.label = "Hi {{fields.zeta}}";
  tagged.blocks[1]!.blocks![0]!.text = "Hello";
  f.send({ type: "document", version: 1, email: tagged });
  f.send({ type: "bar", state: "Draft", acts: ["saveLayout", "publish"] });
  const unfocused = (what: string, update: () => void) => {
    f.away();
    const before = f.focuses.length;
    update();
    assert.equal(f.focuses.length, before, what);
    f.back();
  };
  // A focused outline row, drawn again by a document.
  f.select("headline");
  f.row("headline").focus();
  unfocused("an outline row", () => f.send({ type: "document", version: 2, email: tagged }));
  // A focused act, drawn again by a bar with other acts.
  f.el("bar-acts").querySelectorAll<HTMLElement>("button")[1]!.focus();
  unfocused("an act", () => f.send({ type: "bar", state: "Unsaved changes", acts: ["publish"] }));
  // A focused sample value, moved when a merge tag that sorts before it arrives.
  f.el("samples").querySelector<HTMLInputElement>("input")!.focus();
  const twoTags = clone(tagged);
  twoTags.blocks[0]!.text = "Hi {{displayName}}";
  unfocused("a sample value", () => f.send({ type: "document", version: 3, email: twoTags }));
  // Removing a block: the neighbour is selected, but the focus stays where the person left it.
  f.select("headline");
  f.el("remove-block").focus();
  f.el("remove-block").click();
  const removed = clone(twoTags);
  removed.blocks.splice(0, 1);
  unfocused("the removed block's neighbour", () => f.send({ type: "document", version: 4, email: removed }));
  assert.equal(f.row("body").getAttribute("aria-selected"), "true");
  // Adding a block: the host selects it, but its first field is not focused from afar.
  f.el("add-after").click();
  f.el("add-menu").querySelectorAll<HTMLElement>('[role="menuitem"]')[1]!.click();
  const added = clone(removed);
  added.blocks.splice(1, 0, { id: "text-1", type: "text", text: "" } as never);
  unfocused("the added block's field", () => { f.send({ type: "document", version: 5, email: added }); f.send({ type: "select", block: "text-1" }); });
  assert.equal(f.row("text-1").getAttribute("aria-selected"), "true");
  f.dom.window.close();
});

test("a dimmed preview takes no clicks, only a newer failure dims it, and a click outside the email's blocks selects nothing", () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  const withGhost = rendered.replace("</table></body>", '<tr><td data-block="ghost">Gone</td></tr></table></body>');
  f.send({ type: "render", seq: 5, html: withGhost, text: "Hi", subject: "Hello", issues: [] });
  f.send({ type: "renderFailed", seq: 4, problem: { message: "Older" } });
  assert.ok(!f.el("preview").classList.contains("stale"), "a failure older than the render on its way");
  const frame = f.loadPreview();
  f.send({ type: "renderFailed", seq: 3, problem: { message: "Older still" } });
  assert.ok(!f.el("preview").classList.contains("stale"), "a failure older than the render shown");
  assert.equal(f.visible("preview-notice"), false);
  const click = (id: string) => {
    const event = new f.dom.window.MouseEvent("click", { bubbles: true, cancelable: true });
    frame.contentDocument!.querySelector(`[data-block="${id}"]`)!.dispatchEvent(event);
    return event;
  };
  assert.equal(click("ghost").defaultPrevented, true);
  assert.equal(f.doc.querySelector('[aria-selected="true"]'), null, "a block the document no longer has selects nothing");
  // A newer failure: the last good preview may show blocks whose ids now name others (an added block reuses a free id).
  f.send({ type: "renderFailed", seq: 6, problem: { message: "Newer" } });
  assert.ok(f.el("preview").classList.contains("stale"));
  assert.equal(click("cta").defaultPrevented, true);
  assert.equal(f.doc.querySelector('[aria-selected="true"]'), null, "a dimmed preview selects nothing");
  f.send({ type: "render", seq: 7, html: rendered, text: "Hi", subject: "Hello", issues: [] });
  const next = f.loadPreview();
  next.contentDocument!.querySelector('[data-block="cta"]')!.dispatchEvent(new f.dom.window.MouseEvent("click", { bubbles: true, cancelable: true }));
  assert.equal(f.row("cta").getAttribute("aria-selected"), "true");
  f.dom.window.close();
});

test("a restored selection stays in the page state until its block arrives, and the person's own choice replaces it", () => {
  const f = fixture({ width: "mobile", selected: "cta" });
  assert.deepEqual(f.state(), { width: "mobile", selected: "cta" });
  f.send({ type: "document", version: 1, email: { ...clone(article), blocks: [clone(article).blocks[0]] } });
  assert.deepEqual(f.state(), { width: "mobile", selected: "cta" });
  f.send({ type: "document", version: 2, email: article });
  assert.equal(f.row("cta").getAttribute("aria-selected"), "true");
  assert.deepEqual(f.state(), { width: "mobile", selected: "cta" });
  const g = fixture({ width: "desktop", selected: "later" });
  g.send({ type: "document", version: 1, email: article });
  g.select("headline");
  const later = clone(article);
  later.blocks.push({ id: "later", type: "divider" } as never);
  g.send({ type: "document", version: 2, email: later });
  assert.equal(g.row("headline").getAttribute("aria-selected"), "true");
  f.dom.window.close(); g.dom.window.close();
});

test("the action bar keeps what has not changed, takes a new act after a context, and gives the focus to its state after an act", () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  f.send({ type: "bar", state: "Draft", acts: ["saveLayout", "publish"], layoutName: "Welcome" });
  const words = f.el("bar-state").firstChild;
  const buttons = [...f.el("bar-acts").children];
  f.send({ type: "bar", state: "Draft", acts: ["saveLayout", "publish"], layoutName: "Welcome" });
  assert.equal(f.el("bar-state").firstChild, words, "the live region is not written again");
  assert.deepEqual([...f.el("bar-acts").children], buttons, "the acts stay the same buttons");
  f.send({ type: "bar", state: "Changes since publishing", acts: ["saveLayout", "publish"] });
  assert.equal(f.el("bar-state").textContent, "Changes since publishing");
  assert.deepEqual([...f.el("bar-acts").children], buttons);
  f.send({ type: "bar", state: "Published to Acme Co", acts: ["saveLayout"] });
  assert.deepEqual([...f.el("bar-acts").children], [buttons[0]]);
  // After an act that hides itself, the focus is on the state, not lost to the page.
  f.send({ type: "bar", state: "Draft", acts: ["saveLayout", "publish"] });
  const publish = f.el("bar-acts").querySelectorAll<HTMLElement>("button")[1]!;
  publish.focus();
  publish.click();
  assert.equal(f.doc.activeElement, f.el("bar-state"));
  f.send({ type: "bar", state: "Draft", acts: ["saveLayout", "publish"], layoutName: "Welcome" });
  f.el("bar-acts").querySelector<HTMLElement>("button")!.click();
  f.key(f.el("layout-name"), "Enter");
  assert.deepEqual(f.messages.at(-1), { type: "saveLayout", name: "Welcome" });
  assert.equal(f.doc.activeElement, f.el("bar-state"));
  // A context answers an act too: a cancelled Connect leaves the next click working.
  f.send({ type: "bar", state: "Not connected", acts: ["connect"] });
  f.el("bar-acts").querySelector<HTMLElement>("button")!.click();
  f.send({ type: "context", connected: false, choices: [] });
  f.el("bar-acts").querySelector<HTMLElement>("button")!.click();
  assert.equal(f.messages.filter(m => m.type === "act" && m.act === "connect").length, 2);
  // A problem or a failed render with no words says the page could not show the email, never nothing.
  f.send({ type: "problem", problem: { message: "" } });
  assert.equal(f.el("bar-notice").textContent, C.renderFailed);
  f.send({ type: "renderFailed", seq: 1, problem: { message: "" } });
  assert.equal(f.el("preview-notice").textContent, C.renderFailed);
  f.dom.window.close();
});

test("a problem forgets the selection, the add and the removal it answered", () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  f.select("headline");
  f.el("add-after").click();
  f.el("add-menu").querySelectorAll<HTMLElement>('[role="menuitem"]')[1]!.click();
  f.send({ type: "problem", problem: { message: "Fix the email in Source first." } });
  // The add never happened: a later block arriving from Source is selected by the host only if it says so, without taking the focus to a field.
  f.send({ type: "select", block: "text-9" });
  f.send({ type: "problem", problem: { message: "Fix the email in Source first." } });
  const later = clone(article);
  later.blocks.push({ id: "text-9", type: "text", text: "From Source" } as never);
  f.send({ type: "document", version: 2, email: later });
  assert.equal(f.row("headline").getAttribute("aria-selected"), "true", "the selection the problem answered is forgotten");
  assert.equal(f.doc.activeElement, f.el("add-after"));
  // A refused removal: when Source later removes the block, the focus is not taken to a row.
  f.el("remove-block").focus();
  f.el("remove-block").click();
  f.send({ type: "problem", problem: { message: "Fix the email in Source first." } });
  const gone = clone(later);
  gone.blocks.splice(0, 1);
  f.send({ type: "document", version: 3, email: gone });
  assert.equal(f.doc.activeElement, f.el("remove-block"));
  f.dom.window.close();
});

test("the lines field sends no trailing empty lines, and sample values are not moved when nothing changed", () => {
  const f = fixture();
  const footed = { ...clone(article), blocks: [{ id: "foot", type: "footer", lines: ["Acme Co"], links: [] }, { id: "zeta", type: "heading", text: "Hi {{fields.zeta}}" }] };
  f.send({ type: "document", version: 1, email: footed });
  f.select("foot");
  const lines = f.el("field-lines") as HTMLTextAreaElement;
  lines.focus(); f.type(lines, "Acme Co\nMain Street\n\n"); lines.blur();
  assert.deepEqual(f.edits().at(-1), { kind: "field", block: "foot", field: "lines", value: ["Acme Co", "Main Street"] });
  // A tag that sorts before the one already there: the inputs follow the tags' order, and stay put after that.
  const both = clone(footed);
  both.blocks.push({ id: "name", type: "heading", text: "{{displayName}}" } as never);
  f.send({ type: "document", version: 2, email: both });
  const inputs = [...f.el("samples").querySelectorAll("input")];
  assert.deepEqual(inputs.map(input => input.dataset.sample), ["displayName", "fields.zeta"]);
  let moved = 0;
  new f.dom.window.MutationObserver(records => { moved += records.length; }).observe(f.el("sample-fields"), { childList: true, subtree: true });
  f.send({ type: "document", version: 3, email: both });
  f.send({ type: "document", version: 4, email: both });
  return new Promise<void>(resolve => setTimeout(() => {
    assert.equal(moved, 0, "the same tags leave the inputs where they are");
    assert.deepEqual([...f.el("samples").querySelectorAll("input")], inputs);
    f.dom.window.close();
    resolve();
  }, 5));
});

test("a refused draft gives way to a newer document while the person works in another window", () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  const subject = f.el("subject") as HTMLInputElement;
  subject.focus();
  f.type(subject, "");
  // Split view: the person clicks into the Source editor; leaving the field refuses the empty subject.
  f.away();
  assert.equal(f.el("subject-notice").textContent, "Enter a subject on one line.");
  assert.equal(f.edits().length, 0);
  // The same document again is nothing new: the draft and its notice stay.
  f.send({ type: "document", version: 1, email: article });
  assert.equal(subject.value, "");
  assert.equal(f.el("subject-notice").textContent, "Enter a subject on one line.");
  // An edit made in Source arrives: it replaces the draft, and the refusal goes with it.
  f.send({ type: "document", version: 2, email: { ...clone(article), subject: "From Source" } });
  assert.equal(subject.value, "From Source");
  assert.equal(f.el("subject-notice").textContent, "");
  assert.equal(subject.hasAttribute("aria-invalid"), false);
  // Back in the page, leaving the field sends nothing: the draft is gone.
  f.back();
  subject.focus(); subject.blur();
  assert.equal(f.edits().length, 0);
  f.dom.window.close();
});

test("a refused draft and its notice stay while the person is in the page", () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  const subject = f.el("subject") as HTMLInputElement;
  subject.focus();
  f.type(subject, "");
  f.el("preheader").focus();
  assert.equal(f.el("subject-notice").textContent, "Enter a subject on one line.");
  f.send({ type: "document", version: 2, email: { ...clone(article), subject: "From Source" } });
  assert.equal(subject.value, "", "the person's draft stays");
  assert.equal(f.el("subject-notice").textContent, "Enter a subject on one line.");
  assert.equal(subject.getAttribute("aria-invalid"), "true");
  f.dom.window.close();
});

test("a colour still being picked survives the echo of the one before it while the person is elsewhere", () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  const page = f.el("theme-page") as HTMLInputElement;
  page.focus();
  f.type(page, "#112233");
  f.advance(250);
  assert.deepEqual(f.edits(), [{ kind: "theme", key: "page", value: "#112233" }]);
  // The system colour panel takes the keyboard; the picker moves on before its pause.
  f.away();
  f.type(page, "#445566");
  const echoed = clone(article);
  echoed.theme.page = "#112233";
  f.send({ type: "document", version: 2, email: echoed });
  assert.equal(page.value, "#445566", "a pick with no refusal showing is the person's, not a draft to discard");
  f.advance(250);
  assert.deepEqual(f.edits().at(-1), { kind: "theme", key: "page", value: "#445566" });
  // The picker's commit alone sends too, at once.
  page.value = "#aa00aa";
  page.dispatchEvent(new f.dom.window.Event("change", { bubbles: true }));
  assert.deepEqual(f.edits().at(-1), { kind: "theme", key: "page", value: "#aa00aa" });
  f.dom.window.close();
});

test("after a refused Add, a selection the host makes for its own reasons takes no focus", () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  f.select("headline");
  f.el("add-after").click();
  f.el("add-menu").querySelectorAll<HTMLElement>('[role="menuitem"]')[1]!.click();
  f.send({ type: "problem", problem: { message: "Fix the email in Source first." } });
  const before = f.focuses.length;
  f.send({ type: "select", block: "cta" });
  assert.equal(f.row("cta").getAttribute("aria-selected"), "true");
  assert.equal(f.focuses.length, before, "the refused Add no longer waits for its block");
  assert.equal(f.doc.activeElement, f.el("add-after"));
  f.dom.window.close();
});

test("a restore and a context update what the person left for another window, and spare what they are using", () => {
  const f = fixture();
  f.send({ type: "document", version: 1, email: article });
  const sample = f.el("samples").querySelector<HTMLInputElement>("input")!;
  sample.focus();
  f.type(sample, "typed");
  f.away();
  f.send({ type: "restore", state: { width: "desktop", samples: { displayName: "From state" } } });
  assert.equal(sample.value, "From state", "a value left for another window takes the restored one");
  f.back();
  sample.focus();
  f.type(sample, "typing now");
  f.send({ type: "restore", state: { width: "desktop", samples: { displayName: "From state 2" } } });
  assert.equal(sample.value, "typing now", "a value being typed is not overwritten");
  const choices = [{ id: "a", name: "Acme" }, { id: "b", name: "Beta" }];
  f.send({ type: "context", connected: true, organization: { id: "a", name: "Acme" }, choices });
  const select = f.el("organization") as HTMLSelectElement;
  select.focus();
  f.away();
  f.send({ type: "context", connected: true, organization: { id: "b", name: "Beta" }, choices: [...choices, { id: "c", name: "Cora" }] });
  assert.deepEqual([...select.options].map(option => option.textContent), ["Acme", "Beta", "Cora"], "a select left for another window takes the new choices");
  assert.equal(select.value, "b");
  f.back();
  select.focus();
  f.send({ type: "context", connected: true, organization: { id: "c", name: "Cora" }, choices: [choices[0]!, { id: "c", name: "Cora" }] });
  assert.deepEqual([...select.options].map(option => option.textContent), ["Acme", "Beta", "Cora"], "a select in use is not rebuilt under the person's hand");
  f.dom.window.close();
});
