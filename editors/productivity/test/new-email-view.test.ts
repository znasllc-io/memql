import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { JSDOM } from "jsdom";
import { emailNameProblem, newEmailPage } from "../src/newEmailPage.js";
import { EMAIL_COPY, fill } from "../src/emailCopy.js";

const C = EMAIL_COPY.newEmail;

/** `globals` adds to the window what the webview has and jsdom lacks (a ResizeObserver, say) before the view runs. */
function fixture(saved?: unknown, globals: Record<string, unknown> = {}) {
  const dom = new JSDOM(newEmailPage({ script: "", nonce: "test" }), { runScripts: "outside-only" });
  Object.assign(dom.window, globals);
  // Messages cross a structured clone in the editor; JSON does the same here and brings them into this realm.
  const messages: any[] = [];
  let state = saved;
  Object.assign(dom.window, {
    acquireVsCodeApi: () => ({ postMessage: (m: any) => messages.push(JSON.parse(JSON.stringify(m))), getState: () => state, setState: (value: unknown) => { state = JSON.parse(JSON.stringify(value)); } }),
  });
  // Every focus() call, the page's and the test's: a host update must never add one while the window is elsewhere.
  const focuses: Element[] = [];
  const focus = dom.window.HTMLElement.prototype.focus;
  dom.window.HTMLElement.prototype.focus = function (this: HTMLElement, ...options: [FocusOptions?]) { focuses.push(this); return focus.apply(this, options); };
  dom.window.eval(readFileSync("dist-test/new-email-view.js", "utf8"));
  const doc = dom.window.document, el = (id: string) => doc.getElementById(id)!;
  // The person clicks into another window: the page keeps its active element, but the window no longer has the keyboard.
  const away = () => { Object.assign(doc, { hasFocus: () => false }); };
  const back = () => { delete (doc as unknown as { hasFocus?: unknown }).hasFocus; };
  const send = (data: any) => dom.window.dispatchEvent(new dom.window.MessageEvent("message", { data, origin: dom.window.origin }));
  const type = (target: string | Element, value: string) => {
    const field = (typeof target === "string" ? el(target) : target) as HTMLInputElement;
    field.value = value; field.dispatchEvent(new dom.window.Event("input", { bubbles: true }));
  };
  const key = (target: Element, name: string, init: KeyboardEventInit = {}) => target.dispatchEvent(new dom.window.KeyboardEvent("keydown", { key: name, bubbles: true, cancelable: true, ...init }));
  const cards = () => [...doc.querySelectorAll<HTMLElement>('#layouts [role="radio"]')];
  // Layout ids come from the host: the test, like the page, only ever compares them.
  const card = (id: string) => cards().find(item => item.dataset.id === id)!;
  const named = (element: Element, attribute = "aria-labelledby") => doc.getElementById(element.getAttribute(attribute) ?? "")?.textContent;
  const buttons = () => [...el("bar-acts").querySelectorAll<HTMLButtonElement>("button")];
  const acts = () => buttons().map(button => [button.textContent, button.classList.contains("primary") ? "button" : button.classList.contains("text-act") ? "text" : "?"]);
  const act = (words: string) => buttons().find(button => button.textContent === words)!;
  const barState = () => el("bar-state").textContent;
  const visible = (target: string | Element) => { const element = typeof target === "string" ? el(target) : target; return element.isConnected && !element.closest("[hidden]"); };
  const posted = (kind: string) => messages.filter(m => m.type === kind);
  // jsdom ignores srcdoc: write a thumbnail's srcdoc into its frame, then fire its load event.
  const loadThumbnail = (frame: HTMLIFrameElement) => {
    const d = frame.contentDocument!; d.open(); d.write(frame.getAttribute("srcdoc")!); d.close();
    frame.dispatchEvent(new dom.window.Event("load"));
  };
  return { dom, doc, el, send, type, key, cards, card, named, acts, act, barState, visible, posted, loadThumbnail, messages, focuses, away, back, state: () => state };
}

const SVG = (label: string) => `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 600 780" width="600" height="780"><rect width="600" height="780" fill="#fff"/><title>${label}</title></svg>`;
const BUILTINS = [
  { id: "builtin:confirmation", name: "Confirmation", category: "Welcome", description: "Asks a new subscriber to confirm their address.", wireframe: SVG("Confirmation") },
  { id: "builtin:letter", name: "Letter", category: "Letters", description: "A personal note with a signature.", wireframe: SVG("Letter") },
  { id: "builtin:article", name: "Article", category: "Articles", description: "A banner, a headline, the story and a button.", wireframe: SVG("Article") },
  { id: "builtin:spotlight", name: "Spotlight", category: "Promotions", description: "One offer, front and centre.", wireframe: SVG("Spotlight") },
];
const acme = { id: "acme", name: "Acme Co" };
const client = { id: "client-org", name: "Client Example" };
const setup = (extra: Record<string, unknown> = {}) => ({ type: "setup", connected: true, domain: "acme.example", organizations: [acme], organizationId: "acme", layouts: BUILTINS, dictation: false, ...extra });
const description = "Welcome the autumn subscribers and point them to the new guide.";
const problem = { message: "The AI couldn’t finish within the time allowed. Try a smaller request, or choose another model in Fleet.", reference: "tools-4f7c2a" };

/** A page ready to write: Article chosen, named and described. */
function ready() {
  const f = fixture();
  f.send(setup());
  f.card("builtin:article").click();
  f.type("name", "Autumn welcome");
  f.type("description", description);
  return f;
}

test("choosing a layout and a name makes it ready; a description adds Write it", () => {
  const f = fixture();
  f.send(setup());
  assert.equal(f.barState(), C.chooseLayout);
  assert.deepEqual(f.acts(), []);
  assert.equal(f.el("layouts").getAttribute("role"), "radiogroup");
  assert.equal(f.named(f.el("layouts")), C.layout);
  assert.deepEqual(f.cards().map(card => f.named(card)), ["Confirmation", "Letter", "Article", "Spotlight"]);
  assert.deepEqual(f.cards().map(card => f.named(card, "aria-describedby")), BUILTINS.map(layout => layout.description));
  // Groups are quiet headings, in the engine's category order.
  assert.deepEqual([...f.el("groups").querySelectorAll('[role="group"]')].map(group => f.named(group)), ["Welcome", "Letters", "Articles", "Promotions"]);
  assert.deepEqual(f.cards().map(card => card.getAttribute("aria-checked")), ["false", "false", "false", "false"]);
  assert.equal(f.el("layouts").closest(".loading"), null, "the cards stand still once the layouts are here");
  f.card("builtin:article").click();
  assert.deepEqual(f.cards().map(card => card.getAttribute("aria-checked")), ["false", "false", "true", "false"]);
  assert.equal(f.barState(), C.nameIt);
  assert.deepEqual(f.acts(), []);
  f.type("name", "Autumn welcome");
  assert.equal(f.barState(), C.ready);
  assert.deepEqual(f.acts(), [[C.start, "button"]], "one act: Start from the layout is the button");
  f.type("description", description);
  assert.equal(f.barState(), C.ready);
  assert.deepEqual(f.acts(), [[C.start, "text"], [C.write, "button"]], "with a description, Write it is the button, last");
  // Only words count as a description.
  f.type("description", "  \n ");
  assert.deepEqual(f.acts(), [[C.start, "button"]]);
  // Start from the layout asks for a blank email: the layout and the name only.
  f.act(C.start).click();
  assert.deepEqual(f.posted("start"), [{ type: "start", layoutId: "builtin:article", name: "Autumn welcome" }]);
  // The page reports each state it shows, once.
  assert.deepEqual(f.posted("shown").map(m => [m.state, m.loading]), [[C.loading, true], [C.chooseLayout, false], [C.nameIt, false], [C.ready, false], [C.starting, false]]);
  f.dom.window.close();
});

test("Write it posts the whole request once and the page waits quietly for the host", () => {
  const f = ready();
  f.act(C.write).click();
  f.act(C.write)?.click();
  assert.deepEqual(f.posted("write"), [{ type: "write", layoutId: "builtin:article", name: "Autumn welcome", description }], "a double click writes once");
  assert.equal(f.doc.activeElement, f.el("bar-state"), "the focus waits on the state, not on an act that may go");
  assert.equal(f.visible("form"), true, "the form stays");
  assert.equal(f.barState(), C.starting);
  assert.ok(f.el("bar-state").classList.contains("busy"));
  assert.deepEqual(f.acts(), [], "no act while the host works");
  f.send({ type: "idle" });
  assert.equal((f.el("name") as HTMLInputElement).value, "Autumn welcome");
  assert.equal((f.el("description") as HTMLTextAreaElement).value, description);
  assert.equal(f.card("builtin:article").getAttribute("aria-checked"), "true");
  assert.equal(f.barState(), C.ready);
  assert.deepEqual(f.acts(), [[C.start, "text"], [C.write, "button"]]);
  // Once the host has answered, the next Write it is a new request.
  f.act(C.write).click();
  assert.equal(f.posted("write").length, 2);
  f.dom.window.close();
});

test("a failure keeps the form filled and shows the problem with its reference", () => {
  const f = ready();
  f.act(C.write).click();
  f.send({ type: "failed", problem });
  assert.equal(f.visible("form"), true);
  assert.equal((f.el("name") as HTMLInputElement).value, "Autumn welcome");
  assert.equal((f.el("description") as HTMLTextAreaElement).value, description);
  assert.equal(f.card("builtin:article").getAttribute("aria-checked"), "true");
  assert.equal(f.barState(), C.failed);
  assert.deepEqual(f.acts(), [[C.start, "text"], [C.write, "button"]], "the same acts as before");
  const notice = f.el("bar-notice");
  assert.equal(f.visible(notice), true);
  assert.equal(notice.getAttribute("role"), "alert");
  assert.ok(notice.classList.contains("notice"));
  assert.match(notice.textContent ?? "", /The AI couldn’t finish/);
  assert.equal(notice.querySelector("summary")?.textContent, "Show details");
  assert.equal(notice.querySelector("code")?.textContent, "tools-4f7c2a");
  notice.querySelector<HTMLButtonElement>("details button")!.click();
  assert.deepEqual(f.messages.at(-1), { type: "copyProblemReference", reference: "tools-4f7c2a" });
  // The failure stays until the person changes something.
  f.send({ type: "references", items: [] });
  assert.equal(f.barState(), C.failed);
  f.type("description", `${description} Keep it short.`);
  assert.equal(f.barState(), C.ready);
  assert.equal(f.visible(notice), false);
  // And the same acts write again.
  f.act(C.write).click();
  assert.deepEqual(f.posted("write").at(-1), { type: "write", layoutId: "builtin:article", name: "Autumn welcome", description: `${description} Keep it short.` });
  f.dom.window.close();
});

test("each card draws the server's wireframe as an image and never as markup", () => {
  const f = fixture();
  f.send(setup());
  const decoded = (card: Element) => { const src = card.querySelector("img")!.getAttribute("src")!; assert.match(src, /^data:image\/svg\+xml;base64,/); return Buffer.from(src.split(",")[1]!, "base64").toString("utf8"); };
  for (const layout of BUILTINS) {
    const card = f.card(layout.id);
    assert.equal(decoded(card), layout.wireframe);
    assert.ok(card.querySelector("img")!.closest('[aria-hidden="true"]'), "the picture is not read out: the card's name is");
    assert.equal(card.querySelector("img")!.getAttribute("alt"), "");
    assert.equal(card.querySelector("svg"), null, "never inlined");
    assert.equal(card.querySelector(".sk"), null, "no skeleton once the layouts are here");
  }
  // The name is under the picture, with nothing else drawn in the card: no description, chip or badge.
  assert.equal(f.card("builtin:article").textContent, "Article" + BUILTINS[2]!.description);
  assert.ok(f.card("builtin:article").querySelector(".sr-only"), "the description is for a screen reader alone");
  // A hostile wireframe and a hostile name stay data: an image source and text.
  const hostile = "<svg xmlns='http://www.w3.org/2000/svg'><script>alert(1)</script></svg>";
  f.send(setup({ layouts: [{ id: "x", name: "<img src=x onerror=alert(1)>", category: "Letters", description: "", wireframe: hostile }] }));
  assert.equal(f.cards().length, 1);
  assert.equal(f.cards()[0]!.querySelectorAll("script, svg").length, 0);
  assert.equal(f.cards()[0]!.querySelectorAll("img").length, 1);
  assert.equal(f.named(f.cards()[0]!), "<img src=x onerror=alert(1)>");
  assert.equal(decoded(f.cards()[0]!), hostile);
  // A layout with no wireframe draws a name only, and one without an id or a name is not listed.
  f.send(setup({ layouts: [{ id: "a", name: "Plain", category: "Letters" }, { id: "", name: "No id" }, { id: "b" }, 5] }));
  assert.deepEqual(f.cards().map(card => f.named(card)), ["Plain"]);
  assert.equal(f.cards()[0]!.querySelector("img"), null);
  f.dom.window.close();
});

test("the grid groups layouts by category in the engine's order; an unknown category goes last, an empty one shows nothing", () => {
  const f = fixture();
  const layout = (id: string, category: string) => ({ id, name: id, category, description: "", wireframe: SVG(id) });
  f.send(setup({ layouts: [layout("p1", "Promotions"), layout("z1", "Zeta"), layout("w1", "Welcome"), layout("p2", "Promotions"), layout("y1", "Yonder"), layout("a1", "Articles")] }));
  const groups = [...f.el("groups").querySelectorAll<HTMLElement>('[role="group"]')];
  assert.deepEqual(groups.map(group => f.named(group)), ["Welcome", "Articles", "Promotions", "Zeta", "Yonder"]);
  assert.deepEqual(groups.map(group => [...group.querySelectorAll<HTMLElement>('[role="radio"]')].map(card => card.dataset.id)), [["w1"], ["a1"], ["p1", "p2"], ["z1"], ["y1"]]);
  assert.deepEqual(f.cards().map(card => card.dataset.id), ["w1", "a1", "p1", "p2", "z1", "y1"], "the keyboard walks the grid in the order it is drawn");
  // The same list again keeps the cards.
  const card = f.card("p1");
  f.send(setup({ layouts: [layout("p1", "Promotions"), layout("w1", "Welcome")] }));
  assert.equal(f.card("p1"), card);
  assert.equal(f.el("groups").querySelectorAll('[role="group"]').length, 2);
  f.dom.window.close();
});

test("the layout cards keep their size: 168 pixel columns from the left, each wireframe shown 168 wide in a 168 by 218 window", () => {
  const f = fixture();
  const style = (element: Element) => f.dom.window.getComputedStyle(element);
  const flat = (value: string) => value.replace(/\s+/g, "");
  // The loading shapes and the cards: the same fixed tracks, never stretched.
  f.send(setup());
  for (const grid of [f.el("placeholders"), f.el("groups").querySelector(".cards")!]) {
    const css = style(grid);
    assert.equal(css.display, "grid");
    assert.equal(flat(css.getPropertyValue("grid-template-columns")), "repeat(auto-fill,168px)");
    assert.equal(css.getPropertyValue("gap"), "16px");
    assert.equal(css.getPropertyValue("justify-content"), "start", "left aligned");
  }
  const thumb = style(f.card("builtin:article").querySelector(".thumb")!);
  assert.deepEqual([thumb.width, thumb.height], ["168px", "218px"]);
  const tile = style(f.el("placeholders").querySelector(".tile")!);
  assert.deepEqual([tile.width, tile.height], ["168px", "218px"], "the loading shape is a card's size too");
  // Straight edges, no boxes: the cards and their shapes are not rounded, and the chosen one is marked by an underline and a soft fill.
  const css = f.doc.querySelector("style")!.textContent!;
  assert.match(css, /\.card\{[^}]*border-radius:0/);
  assert.match(css, /\.tile\{[^}]*border-radius:0/);
  assert.match(css, /\.card\[aria-checked="true"\]\{background:color-mix/);
  assert.match(css, /\.card\[aria-checked="true"\] \.card-name\{[^}]*box-shadow:inset 0 -2px 0 var\(--vscode-focusBorder\)/);
  assert.match(css, /\.bar\{border-top:0\}/, "no divider above the bar");
  assert.match(css, /\.bar-notice\{padding:8px 16px;background:var\(--vscode-editor-background\)\}/, "none above the problem either");
  f.dom.window.close();
});

test("references show as chips; removing posts the index", () => {
  const f = fixture();
  f.send(setup());
  assert.equal(f.el("attach").getAttribute("aria-label"), C.addReferences);
  f.el("attach").click();
  assert.deepEqual(f.messages.at(-1), { type: "attach" });
  f.send({ type: "references", items: [
    { label: "brief.md", kind: "file", uploading: false },
    { label: "logo.png", kind: "picture", uploading: false },
    { label: "brand.zip", kind: "bundle", uploading: false },
    { label: "photo.jpg", kind: "picture", uploading: true },
  ] });
  const chips = () => [...f.el("references").querySelectorAll<HTMLElement>(".chip")];
  assert.deepEqual(chips().map(chip => chip.querySelector(".chip-name")?.textContent), ["brief.md", "logo.png", "brand.zip", "photo.jpg"]);
  assert.equal(f.el("references").querySelector('input'), null, "no switch: every file is material for the email");
  assert.ok(chips()[3]!.querySelector(".spinner"), "an uploading reference shows a spinner");
  assert.equal(chips()[0]!.querySelector(".spinner"), null);
  const remove = chips()[2]!.querySelector<HTMLButtonElement>("button.remove")!;
  assert.equal(remove.getAttribute("aria-label"), fill(C.removeReference, { name: "brand.zip" }));
  remove.click();
  assert.deepEqual(f.messages.at(-1), { type: "removeReference", index: 2 });
  // A label is text, never markup; a problem shows under the references until a list comes without one.
  const hostile = "<img src=x onerror=alert(1)>.png";
  f.send({ type: "references", items: [{ label: hostile, kind: "picture", uploading: false }, { label: 5, kind: "file" }], problem: { message: EMAIL_COPY.host.tooMany } });
  assert.deepEqual(chips().map(chip => chip.querySelector(".chip-name")?.textContent), [hostile]);
  assert.equal(f.el("references").querySelector("img"), null);
  assert.equal(f.el("describe-notice").getAttribute("role"), "alert");
  assert.equal(f.el("describe-notice").textContent, EMAIL_COPY.host.tooMany);
  f.send({ type: "references", items: [] });
  assert.equal(chips().length, 0);
  assert.equal(f.el("describe-notice").textContent, "");
  f.dom.window.close();
});

test("the Organization select appears only with more than one organization and posts a change", () => {
  const f = fixture();
  f.send(setup());
  assert.equal(f.visible("organization-row"), false);
  assert.equal(f.visible("meta"), true);
  assert.equal(f.el("meta").textContent, "Acme Co", "one organization is a quiet line under the title");
  f.send(setup({ organizations: [acme, client], layouts: BUILTINS }));
  assert.equal(f.visible("organization-row"), true);
  assert.equal(f.visible("meta"), false);
  const select = f.el("organization") as HTMLSelectElement;
  assert.equal(f.doc.querySelector('label[for="organization"]')?.textContent, C.organization);
  assert.deepEqual([...select.options].map(option => [option.value, option.textContent]), [["acme", "Acme Co"], ["client-org", "Client Example"]]);
  assert.equal(select.value, "acme");
  f.card("builtin:letter").click();
  select.value = "client-org";
  select.dispatchEvent(new f.dom.window.Event("change", { bubbles: true }));
  assert.deepEqual(f.posted("organization"), [{ type: "organization", id: "client-org" }]);
  // Until the organization's layouts arrive, the page shows the shape of the cards.
  assert.equal(f.barState(), C.loading);
  assert.deepEqual(f.acts(), []);
  assert.equal(f.cards().length, 0);
  assert.ok(f.el("layouts").querySelector(".placeholder"));
  f.send(setup({ organizations: [acme, client], organizationId: "client-org", layouts: BUILTINS }));
  assert.equal(select.value, "client-org");
  assert.equal(f.cards().length, 4);
  assert.equal(f.card("builtin:letter").getAttribute("aria-checked"), "true", "a built-in layout is the same in every organization");
  f.type("name", "Autumn welcome");
  assert.deepEqual(f.posted("draft").at(-1), { type: "draft", draft: { organizationId: "client-org", layoutId: "builtin:letter", name: "Autumn welcome" } });
  // Without a chosen organization, the select asks for one.
  f.send(setup({ organizations: [acme, client], organizationId: undefined, layouts: BUILTINS }));
  assert.deepEqual([...select.options].map(option => [option.value, option.textContent]), [["", C.chooseOrganization], ["acme", "Acme Co"], ["client-org", "Client Example"]]);
  assert.equal(select.value, "");
  assert.equal(f.barState(), C.chooseOrganization);
  // An organization the person does not have is no choice.
  f.send(setup({ organizations: [acme, client], organizationId: "someone-else", layouts: BUILTINS }));
  assert.equal(f.barState(), C.chooseOrganization);
  f.dom.window.close();
});

test("not connected shows only Connect", () => {
  const f = fixture();
  f.send({ type: "setup", connected: false, domain: "acme.example", organizations: [], layouts: [], dictation: true });
  assert.equal(f.barState(), fill(C.notConnected, { domain: "acme.example" }));
  assert.deepEqual(f.acts(), [[C.connect, "button"]]);
  assert.equal(f.visible("form"), false);
  assert.equal(f.visible("no-organization"), false);
  f.act(C.connect).click();
  f.act(C.connect).click();
  assert.deepEqual(f.posted("connect"), [{ type: "connect" }], "a double click connects once");
  f.send({ type: "idle" });
  f.send(setup());
  assert.equal(f.barState(), C.chooseLayout);
  assert.equal(f.visible("form"), true);
  f.dom.window.close();
});

test("arrow keys move the layout choice", () => {
  const f = fixture();
  f.send(setup({ layouts: BUILTINS }));
  const ids = BUILTINS.map(layout => layout.id);
  const checked = () => f.cards().filter(card => card.getAttribute("aria-checked") === "true").map(card => card.dataset.id);
  const stops = () => f.cards().filter(card => card.tabIndex === 0).map(card => card.dataset.id);
  assert.deepEqual(stops(), [ids[0]], "one tab stop: the first card until one is chosen");
  f.cards()[0]!.focus();
  f.key(f.cards()[0]!, "ArrowRight");
  assert.deepEqual(checked(), [ids[1]]);
  assert.deepEqual(stops(), [ids[1]]);
  assert.equal(f.doc.activeElement, f.card(ids[1]!));
  // Across the groups: the organization's last layout is followed by the first built-in one.
  f.key(f.card(ids[1]!), "ArrowDown");
  assert.deepEqual(checked(), [ids[2]]);
  assert.equal(f.doc.activeElement, f.card(ids[2]!));
  f.key(f.card(ids[2]!), "ArrowLeft");
  f.key(f.card(ids[1]!), "ArrowUp");
  assert.deepEqual(checked(), [ids[0]]);
  // The ends wrap; Home and End go to them.
  f.key(f.card(ids[0]!), "ArrowLeft");
  assert.deepEqual(checked(), [ids.at(-1)]);
  f.key(f.card(ids.at(-1)!), "ArrowRight");
  assert.deepEqual(checked(), [ids[0]]);
  f.key(f.card(ids[0]!), "End");
  assert.deepEqual(checked(), [ids.at(-1)]);
  f.key(f.card(ids.at(-1)!), "Home");
  assert.deepEqual(checked(), [ids[0]]);
  // Space chooses the card with the focus; other keys do nothing.
  f.card(ids[3]!).focus();
  f.key(f.card(ids[3]!), "a");
  assert.deepEqual(checked(), [ids[0]]);
  f.key(f.card(ids[3]!), " ");
  assert.deepEqual(checked(), [ids[3]]);
  assert.deepEqual(stops(), [ids[3]]);
  // Each choice is kept in the draft.
  assert.deepEqual(f.posted("draft").map(m => m.draft.layoutId), [ids[1], ids[2], ids[1], ids[0], ids.at(-1), ids[0], ids.at(-1), ids[0], ids[3]]);
  f.dom.window.close();
});

test("the draft is posted for keeping and restored on ready", () => {
  const f = fixture();
  assert.equal(f.messages[0]?.type, "ready");
  f.send(setup());
  f.card("builtin:letter").click();
  f.type("name", "Monthly letter");
  f.type("description", "October news");
  const kept = { organizationId: "acme", layoutId: "builtin:letter", name: "Monthly letter", description: "October news" };
  assert.deepEqual(f.posted("draft"), [
    { type: "draft", draft: { organizationId: "acme", layoutId: "builtin:letter" } },
    { type: "draft", draft: { organizationId: "acme", layoutId: "builtin:letter", name: "Monthly letter" } },
    { type: "draft", draft: kept },
  ], "each change is posted at once");
  assert.deepEqual(f.state(), { draft: kept }, "and kept in the webview's own state");
  f.dom.window.close();

  // A new page for the same link: the host restores the draft on ready, before or after the layouts.
  const g = fixture();
  g.send({ type: "restore", draft: kept });
  assert.equal((g.el("name") as HTMLInputElement).value, "Monthly letter");
  assert.equal((g.el("description") as HTMLTextAreaElement).value, "October news");
  g.send(setup());
  assert.equal(g.card("builtin:letter").getAttribute("aria-checked"), "true");
  assert.equal(g.barState(), C.ready);
  assert.deepEqual(g.acts(), [[C.start, "text"], [C.write, "button"]]);
  assert.equal(g.posted("draft").length, 0, "a restore is not posted back");
  g.dom.window.close();

  // The webview's own state brings the draft back at once, before the host answers.
  const h = fixture({ draft: { layoutId: "builtin:article", name: "Autumn welcome" } });
  assert.equal((h.el("name") as HTMLInputElement).value, "Autumn welcome");
  h.send(setup());
  assert.equal(h.card("builtin:article").getAttribute("aria-checked"), "true");
  assert.equal(h.barState(), C.ready);
  h.dom.window.close();

  // A restore never overwrites the field the person is typing in, and a layout the list does not have is not chosen.
  const k = fixture();
  k.send(setup());
  k.el("name").focus();
  k.type("name", "Typing");
  k.send({ type: "restore", draft: { layoutId: "builtin:gone", name: "Stored", description: "Stored description" } });
  assert.equal((k.el("name") as HTMLInputElement).value, "Typing");
  assert.equal((k.el("description") as HTMLTextAreaElement).value, "Stored description");
  assert.equal(k.cards().some(card => card.getAttribute("aria-checked") === "true"), false);
  assert.equal(k.barState(), C.chooseLayout);
  // A restore after the first one is the host repeating itself: what the person did since stays.
  k.type("description", "Mine");
  k.send({ type: "restore", draft: { description: "Older" } });
  assert.equal((k.el("description") as HTMLTextAreaElement).value, "Mine");
  k.dom.window.close();
});

test("a restored organization is asked for once, and its layout chosen when it arrives", () => {
  const f = fixture();
  f.send({ type: "restore", draft: { organizationId: "client-org", layoutId: "builtin:letter", name: "News" } });
  f.send(setup({ organizations: [acme, client], organizationId: "acme", layouts: BUILTINS }));
  assert.deepEqual(f.posted("organization"), [{ type: "organization", id: "client-org" }]);
  assert.equal(f.barState(), C.loading);
  assert.equal((f.el("organization") as HTMLSelectElement).value, "client-org");
  f.send(setup({ organizations: [acme, client], organizationId: "client-org", layouts: BUILTINS }));
  assert.equal(f.card("builtin:letter").getAttribute("aria-checked"), "true");
  assert.equal(f.barState(), C.ready);
  // The host has the last word: a later list for another organization is not argued with.
  f.send(setup({ organizations: [acme, client], organizationId: "acme", layouts: BUILTINS }));
  assert.equal(f.posted("organization").length, 1);
  assert.equal(f.barState(), C.ready);
  f.dom.window.close();
});

test("dictation appears only where the host can dictate", () => {
  const f = fixture();
  f.send(setup());
  const dictate = f.el("dictate") as HTMLButtonElement;
  const words = f.el("description") as HTMLTextAreaElement;
  assert.equal(f.visible(dictate), false);
  f.send(setup({ dictation: true }));
  assert.equal(f.visible(dictate), true);
  assert.equal(dictate.getAttribute("aria-label"), C.dictate);
  assert.equal(dictate.getAttribute("aria-pressed"), "false");
  f.card("builtin:article").click();
  f.type("name", "Autumn welcome");
  f.type(words, "Hello");
  dictate.click();
  assert.deepEqual(f.messages.at(-1), { type: "dictationStart" });
  assert.equal(f.el("dictation-status").textContent, C.openingMicrophone);
  assert.equal(dictate.getAttribute("aria-label"), C.cancelDictation);
  assert.equal(dictate.getAttribute("aria-pressed"), "true");
  assert.equal(words.readOnly, true, "the words wait while dictation writes them");
  f.send({ type: "dictation", phase: "listening" });
  assert.equal(f.el("dictation-status").textContent, C.listening);
  assert.equal(dictate.getAttribute("aria-label"), C.stopDictation);
  f.send({ type: "dictation", phase: "listening", text: "welcome" });
  f.send({ type: "dictation", phase: "listening", text: "welcome everyone" });
  assert.equal(words.value, "Hello welcome everyone", "the transcript so far follows what was there");
  dictate.click();
  assert.deepEqual(f.messages.at(-1), { type: "dictationStop" });
  f.send({ type: "dictation", phase: "transcribing" });
  assert.equal(f.el("dictation-status").textContent, C.transcribing);
  assert.equal(dictate.getAttribute("aria-label"), C.cancelDictation);
  f.send({ type: "dictation", phase: "idle", text: "welcome everyone.", final: true });
  assert.equal(words.value, "Hello welcome everyone.");
  assert.equal(words.readOnly, false);
  assert.equal(f.el("dictation-status").textContent, "");
  assert.equal(dictate.getAttribute("aria-label"), C.dictate);
  assert.deepEqual(f.posted("draft").at(-1)?.draft.description, "Hello welcome everyone.");
  assert.deepEqual(f.acts(), [[C.start, "text"], [C.write, "button"]]);
  // A failure says why, with its reference.
  dictate.click();
  f.send({ type: "dictation", phase: "idle", error: "Microphone access is blocked. Allow it in your browser settings, then try dictating again.", reference: "tools-77aa01" });
  assert.match(f.el("describe-notice").textContent ?? "", /Microphone access is blocked/);
  assert.equal(f.el("describe-notice").querySelector("code")?.textContent, "tools-77aa01");
  assert.equal(f.el("dictation-status").textContent, "");
  // Starting again clears it; a cancel while opening posts dictationCancel; writing mid-dictation cancels it first.
  dictate.click();
  assert.equal(f.el("describe-notice").textContent, "");
  dictate.click();
  assert.deepEqual(f.messages.at(-1), { type: "dictationCancel" });
  f.send({ type: "dictation", phase: "listening", text: "and more" });
  f.act(C.write).click();
  assert.deepEqual(f.messages.filter(m => m.type !== "shown").slice(-2), [{ type: "dictationCancel" }, { type: "write", layoutId: "builtin:article", name: "Autumn welcome", description: "Hello welcome everyone. and more" }]);
  // A transcript the page did not ask for is not written into the description.
  const g = fixture();
  g.send(setup({ dictation: true }));
  g.type("description", "Mine");
  g.send({ type: "dictation", phase: "idle", text: "stray", final: true });
  assert.equal((g.el("description") as HTMLTextAreaElement).value, "Mine");
  g.dom.window.close();
  f.dom.window.close();
});

test("messages from another origin are ignored", () => {
  const f = fixture();
  const forged = (data: unknown) => f.dom.window.dispatchEvent(new f.dom.window.MessageEvent("message", { data, origin: "https://attacker.example" }));
  forged(setup());
  forged({ type: "restore", draft: { name: "Forged" } });
  forged({ type: "references", items: [{ label: "x.png", kind: "picture", uploading: false }] });
  forged({ type: "reference", item: { label: "forged.png" } });
  forged({ type: "layoutsFailed", problem });
  assert.equal(f.cards().length, 0);
  assert.equal((f.el("name") as HTMLInputElement).value, "");
  assert.equal(f.el("references").children.length, 0);
  assert.equal(f.barState(), C.loading);
  assert.deepEqual(f.messages.map(m => m.type), ["ready", "shown"]);
  f.send(setup());
  assert.equal(f.el("finished").children.length, 0);
  assert.equal(f.cards().length, 4);
  f.dom.window.close();
});

test("a name is refused in place only when it has a slash, a control character or more than 150 characters", () => {
  for (const name of ["Autumn welcome", "a", "  Autumn  ", "x".repeat(150), "Q3 · news", "Été"]) assert.equal(emailNameProblem(name), undefined, JSON.stringify(name));
  for (const name of ["", "   ", "a/b", "a\\b", "tab\there", "line\nbreak", "\u0000", "\u001f", "x".repeat(151)]) assert.equal(emailNameProblem(name), C.nameInvalid, JSON.stringify(name));
  const f = fixture();
  f.send(setup());
  f.card("builtin:article").click();
  const name = f.el("name") as HTMLInputElement, notice = f.el("name-notice");
  assert.equal(name.getAttribute("aria-describedby"), "name-notice");
  f.type(name, "Autumn/welcome");
  assert.equal(notice.textContent, C.nameInvalid);
  assert.equal(notice.getAttribute("role"), "alert");
  assert.equal(name.getAttribute("aria-invalid"), "true");
  assert.equal(f.barState(), C.nameIt);
  assert.deepEqual(f.acts(), []);
  f.type(name, "Autumn welcome");
  assert.equal(notice.textContent, "");
  assert.equal(name.hasAttribute("aria-invalid"), false);
  assert.equal(f.barState(), C.ready);
  // A blank name is not wrong, only missing: the bar says so, the field says nothing.
  f.type(name, "   ");
  assert.equal(notice.textContent, "");
  assert.equal(f.barState(), C.nameIt);
  f.dom.window.close();
});

test("loading is the shape of the cards, and a failed list or no organization says so in the bar", () => {
  const f = fixture();
  assert.equal(f.barState(), C.loading);
  assert.equal(f.el("bar-state").querySelector(".sr-only")?.textContent, C.loading, "the words are for a screen reader alone");
  assert.deepEqual(f.acts(), []);
  assert.equal(f.cards().length, 0);
  const placeholders = f.el("layouts").querySelectorAll(".placeholder");
  assert.ok(placeholders.length >= 3);
  assert.ok([...placeholders].every(item => item.closest('[aria-hidden="true"]')));
  assert.ok(f.el("layouts").closest(".loading"), "the shapes sweep");
  assert.equal(f.visible("form"), true, "the name and the description can be typed while the layouts load");
  f.send({ type: "layoutsFailed", problem: { message: "MemQL couldn’t complete the request. Check your connection and try again.", reference: "tools-9a8b7c" } });
  assert.equal(f.barState(), C.layoutsFailed);
  assert.deepEqual(f.acts(), [[C.retry, "button"]]);
  assert.match(f.el("bar-notice").textContent ?? "", /couldn’t complete the request/);
  assert.equal(f.visible("bar-notice"), true);
  assert.equal(f.visible("layout-group"), false);
  f.act(C.retry).click();
  f.act(C.retry)?.click();
  assert.deepEqual(f.posted("retry"), [{ type: "retry" }]);
  assert.equal(f.barState(), C.loading);
  assert.equal(f.visible("bar-notice"), false);
  assert.equal(f.visible("layout-group"), true);
  f.send(setup({ organizations: [], organizationId: undefined }));
  assert.equal(f.barState(), C.noOrganization);
  assert.deepEqual(f.acts(), []);
  assert.equal(f.visible("no-organization"), true);
  assert.equal(f.el("no-organization").textContent, C.noOrganizationLine);
  assert.equal(f.visible("form"), false);
  f.dom.window.close();
});

test("Ctrl or Cmd+Enter in the description writes", () => {
  const f = ready();
  f.key(f.el("description"), "Enter", { ctrlKey: true });
  assert.deepEqual(f.posted("write"), [{ type: "write", layoutId: "builtin:article", name: "Autumn welcome", description }]);
  f.send({ type: "idle" });
  f.key(f.el("description"), "Enter", { metaKey: true });
  assert.equal(f.posted("write").length, 2);
  // A plain Enter is a new line; without a description there is nothing to write.
  f.send({ type: "idle" });
  assert.equal(f.key(f.el("description"), "Enter"), true);
  f.type("description", "");
  f.key(f.el("description"), "Enter", { ctrlKey: true });
  assert.equal(f.posted("write").length, 2);
  assert.equal(f.posted("start").length, 0);
  f.dom.window.close();
});

test("the page is static: every id the view uses, the policy, and only the email copy", () => {
  const html = newEmailPage({ script: "https://file+.vscode-resource.test/out/newEmailView.js", nonce: "abc123" });
  const doc = new JSDOM(html).window.document;
  for (const id of ["meta", "no-organization", "form", "organization-row", "organization", "layout-group", "layouts", "name", "name-notice", "description", "attach", "dictate", "references", "dictation-status", "describe-notice", "groups", "placeholders", "attach-reference", "finished", "bar", "bar-state", "bar-acts", "bar-notice"]) {
    assert.ok(doc.getElementById(id), id);
  }
  assert.equal(doc.title, C.title);
  assert.equal(doc.querySelector("h1")?.textContent, C.title);
  assert.equal(doc.querySelector('meta[http-equiv="Content-Security-Policy"]')?.getAttribute("content"), "default-src 'none'; script-src 'nonce-abc123'; style-src 'unsafe-inline'; img-src data:; frame-src 'self'; form-action 'none'; base-uri 'none'");
  assert.equal(doc.querySelector("script")?.getAttribute("src"), "https://file+.vscode-resource.test/out/newEmailView.js");
  assert.equal(doc.querySelector("form"), null, "no form to submit by accident");
  const words = doc.getElementById("description") as HTMLTextAreaElement;
  assert.equal(words.maxLength, 8000);
  assert.equal(words.placeholder, C.describePlaceholder);
  assert.equal((doc.getElementById("name") as HTMLInputElement).placeholder, C.namePlaceholder);
  assert.equal(doc.getElementById("bar-state")?.getAttribute("role"), "status");
  assert.equal(doc.getElementById("dictation-status")?.getAttribute("role"), "status");
  // Every word a person can see or hear on the page is from EMAIL_COPY.
  const copy = new Set<string>();
  const collect = (value: unknown): void => { if (typeof value === "string") copy.add(value); else if (value && typeof value === "object") Object.values(value).forEach(collect); };
  collect(EMAIL_COPY);
  const walker = doc.createTreeWalker(doc.body, 4);
  const seen: string[] = [];
  for (let node = walker.nextNode(); node; node = walker.nextNode()) if (node.textContent!.trim() && node.parentElement?.localName !== "script") seen.push(node.textContent!.trim());
  for (const element of doc.querySelectorAll("[aria-label],[title],[placeholder]")) for (const name of ["aria-label", "title", "placeholder"]) { const value = element.getAttribute(name); if (value) seen.push(value); }
  assert.ok(seen.length >= 8);
  for (const word of seen) assert.ok(copy.has(word), `"${word}" is not in EMAIL_COPY`);
});

test("a control the keyboard reaches scrolls into view above the bar, which the page measures as it grows", () => {
  const observed: { target: Element; callback: () => void }[] = [];
  class FakeResizeObserver {
    constructor(private readonly callback: () => void) {}
    observe(target: Element) { observed.push({ target, callback: this.callback }); }
    disconnect() {}
  }
  const f = fixture(undefined, { ResizeObserver: FakeResizeObserver });
  assert.equal(observed.length, 1);
  const dock = observed[0]!.target as HTMLElement;
  assert.ok(dock.contains(f.el("bar")) && dock.contains(f.el("bar-notice")), "the bar and the problem above it are measured together");
  assert.equal(f.doc.documentElement.style.getPropertyValue("--dock"), "0px", "measured at once (jsdom lays nothing out), not only on the first resize");
  (dock as unknown as { getBoundingClientRect: () => { height: number } }).getBoundingClientRect = () => ({ height: 96.4 });
  observed[0]!.callback();
  assert.equal(f.doc.documentElement.style.getPropertyValue("--dock"), "97px");
  // A problem shown above the bar is measured as it is drawn, before any resize is reported.
  (dock as unknown as { getBoundingClientRect: () => { height: number } }).getBoundingClientRect = () => ({ height: 140 });
  f.send({ type: "layoutsFailed", problem });
  assert.equal(f.visible("bar-notice"), true);
  assert.equal(f.doc.documentElement.style.getPropertyValue("--dock"), "140px");
  // A text field scrolls only to its caret as it takes the focus: the description asks for all of itself to be in view.
  const asked: unknown[] = [];
  Object.assign(f.el("description"), { scrollIntoView: (options: unknown) => asked.push(options) });
  f.el("description").focus();
  assert.deepEqual(JSON.parse(JSON.stringify(asked)), [{ block: "nearest" }]);
  // The page's controls read it: a focused card or field stops 8 pixels above the bar, its outline in view. It is their
  // own scroll margin, not the window's scroll padding, which would scroll the page each time an act in the bar is focused.
  const css = f.doc.querySelector("style")!.textContent!;
  assert.match(css, /\.page :is\(a,button,input,select,textarea,\[tabindex\]\)\{scroll-margin:8px 0 calc\(var\(--dock,48px\) \+ 8px\)\}/);
  assert.doesNotMatch(css, /scroll-padding/);
  // Words that say what is optional keep the muted colour's contrast: they are not faded further.
  assert.doesNotMatch(css, /\.optional\{[^}]*opacity/);
  f.dom.window.close();
});

test("no host update takes focus while the window is elsewhere, and the page keeps the focus it holds", () => {
  const f = fixture();
  f.send(setup({ layouts: BUILTINS, dictation: true }));
  const unfocused = (what: string, update: () => void) => {
    f.away();
    const before = f.focuses.length;
    update();
    assert.equal(f.focuses.length, before, what);
    f.back();
  };
  // A focused card, its list sent again with one layout fewer.
  f.card("builtin:letter").focus();
  unfocused("a card", () => f.send(setup({ layouts: BUILTINS, dictation: true })));
  // A focused reference, removed by the host.
  f.send({ type: "references", items: [{ label: "a.md", kind: "file", uploading: false }, { label: "b.md", kind: "file", uploading: false }] });
  f.el("references").querySelector<HTMLElement>(".chip button")!.focus();
  unfocused("a reference", () => f.send({ type: "references", items: [{ label: "b.md", kind: "file", uploading: false }] }));
  // A focused field, with the host answering an act.
  f.el("description").focus();
  unfocused("the description", () => f.send({ type: "failed", problem }));
  unfocused("the form coming back", () => f.send({ type: "idle" }));
  // While the page holds the focus, a card drawn again keeps it, and so does a reference.
  f.card("builtin:article").focus();
  f.send(setup({ layouts: BUILTINS, dictation: true }));
  assert.equal(f.doc.activeElement, f.card("builtin:article"));
  f.send({ type: "references", items: [{ label: "a.md", kind: "file", uploading: false }, { label: "b.md", kind: "file", uploading: false }] });
  const removeFirst = f.el("references").querySelector<HTMLElement>(".chip button")!;
  removeFirst.focus();
  removeFirst.click();
  f.send({ type: "references", items: [{ label: "b.md", kind: "file", uploading: false }] });
  assert.equal(f.doc.activeElement, f.el("references").querySelector(".chip button"), "the next reference takes the focus of the one removed");
  f.send({ type: "references", items: [] });
  assert.equal(f.doc.activeElement, f.el("attach"), "the last one gives it to Add references");
  f.dom.window.close();
});


test("organization changes save the draft and refresh the select after blur", () => {
  const f = fixture(); f.send(setup({ organizations: [acme, { id: "other", name: "Other" }], organizationId: undefined }));
  const select = f.el("organization") as HTMLSelectElement; select.focus(); select.value = "other";
  select.dispatchEvent(new f.dom.window.Event("change", { bubbles: true }));
  assert.equal(f.posted("draft").at(-1).draft.organizationId, "other");
  f.send(setup({ organizations: [acme, { id: "other", name: "Other" }], organizationId: "other" }));
  f.el("name").focus(); assert.equal(select.querySelector('option[value=""]'), null); f.dom.window.close();
});

test("an unrelated setup cannot reopen an in-flight write; a card names its layout", () => {
  const f = fixture(); f.send(setup()); f.card("builtin:article").click(); f.type("name", "Welcome"); f.type("description", "Write a welcome.");
  f.act(C.write).click(); f.send(setup()); f.act(C.write)?.click(); assert.equal(f.posted("write").length, 1);
  f.send({ type: "references", items: [{ label: "logo.png", kind: "picture" }] });
  assert.equal(f.card("builtin:article").querySelector('.card-name')?.getAttribute("title"), `Article. ${BUILTINS[2]!.description}`); f.dom.window.close();
});

test("the picture of the finished email is one optional picture, apart from the files", () => {
  const f = fixture();
  f.send(setup());
  const label = f.doc.getElementById("finished-label")!;
  assert.equal(label.textContent?.startsWith(C.finishedPicture), true);
  const add = f.el("attach-reference");
  assert.equal(f.visible(add), true);
  assert.equal(add.getAttribute("aria-label"), C.addFinishedPicture);
  add.click();
  assert.deepEqual(f.messages.at(-1), { type: "attachReference" });
  f.send({ type: "reference", item: { label: "mock.png" } });
  assert.equal(f.visible(add), false, "one picture: nothing to add once it is here");
  assert.equal(f.el("finished").querySelector(".chip-name")?.textContent, "mock.png");
  assert.equal(f.el("references").children.length, 0, "it is not one of the files");
  const remove = f.el("finished").querySelector<HTMLButtonElement>("button.remove")!;
  assert.equal(remove.getAttribute("aria-label"), C.removeFinishedPicture);
  remove.click();
  assert.deepEqual(f.messages.at(-1), { type: "clearReference" });
  f.send({ type: "reference", item: null });
  assert.equal(f.el("finished").children.length, 0);
  assert.equal(f.visible(add), true);
  // A refusal says why under the description; a label is text.
  f.send({ type: "reference", item: null, problem: { message: EMAIL_COPY.host.pictureType } });
  assert.equal(f.el("describe-notice").textContent, EMAIL_COPY.host.pictureType);
  f.send({ type: "reference", item: { label: "<b>x</b>.png" } });
  assert.equal(f.el("finished").querySelector("b"), null);
  assert.equal(f.el("describe-notice").textContent, "");
  f.dom.window.close();
});

test("Start from the layout is shown with a layout and a name, Write it only with a description too; neither before a layout", () => {
  const f = fixture();
  f.send(setup());
  f.type("name", "Autumn welcome"); f.type("description", description);
  assert.deepEqual(f.acts(), [], "no act until a layout is chosen");
  f.card("builtin:letter").click();
  assert.deepEqual(f.acts().map(a => a[0]), [C.start, C.write]);
  f.type("description", "");
  assert.deepEqual(f.acts().map(a => a[0]), [C.start], "Write it is absent, not disabled");
  assert.equal(f.dom.querySelector?.("button:disabled") ?? null, null);
  f.dom.window.close();
});
