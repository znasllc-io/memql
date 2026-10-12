import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { JSDOM } from "jsdom";
import { emailNameProblem, newEmailPage } from "../src/newEmailPage.js";
import { EMAIL_COPY, fill } from "../src/emailCopy.js";
import { previewHTML, skeletonHTML } from "../src/emailPreview.js";
import type { EmailBlock } from "../src/emailDocument.js";

const C = EMAIL_COPY.newEmail;

function fixture(saved?: unknown) {
  const dom = new JSDOM(newEmailPage({ script: "", nonce: "test" }), { runScripts: "outside-only" });
  // Messages cross a structured clone in the editor; JSON does the same here and brings them into this realm.
  const messages: any[] = [];
  let state = saved;
  // A clock the test moves by hand, so a pause is exact and the suite never sleeps.
  let now = 0, nextTimer = 1;
  const timers = new Map<number, { at: number; run: () => void }>();
  Object.assign(dom.window, {
    setTimeout: (run: () => void, ms = 0) => { const id = nextTimer++; timers.set(id, { at: now + ms, run }); return id; },
    clearTimeout: (id: number) => { timers.delete(id); },
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
  return { dom, doc, el, send, advance, type, key, cards, card, named, acts, act, barState, visible, posted, loadThumbnail, messages, focuses, away, back, state: () => state };
}

const theme = { page: "#eef0f3", canvas: "#ffffff", primary: "#2f5d8a", secondary: "#6f93b8", accent: "#d9822b", tint: "#f5f4f0", text: "#2b2b2b", muted: "#8a8a8a", headingFont: "Georgia", bodyFont: "Helvetica" };
const layoutDocument = (layout: string, blocks: EmailBlock[]) => ({ subject: "Hello from Acme", preheader: "A short note", layout, theme, blocks });
const logo: EmailBlock = { id: "logo", type: "image", role: "brand", src: "", alt: "Acme", width: 120, height: 40 };
const footer: EmailBlock = { id: "footer", type: "footer", role: "brand", lines: ["Acme Co", "1 Example Street"] };
const ARTICLE = layoutDocument("builtin:article", [logo,
  { id: "banner", type: "image", src: "", alt: "Banner", width: 600, height: 200 },
  { id: "headline", type: "heading", text: "Three ways to get more from your plan", size: 30 },
  { id: "intro", type: "text", text: "Most busy mornings start the same way. Here are three small changes our customers swear by." },
  { id: "cta", type: "button", label: "Read the guide", href: "https://acme.example/guide" }, footer]);
const BUILTINS = [
  { id: "builtin:confirmation", name: "Confirmation", description: "Asks a new subscriber to confirm their address.", kind: "builtin",
    document: layoutDocument("builtin:confirmation", [logo, { id: "headline", type: "heading", text: "Confirm your address" }, { id: "cta", type: "button", label: "Confirm", href: "{{confirmUrl}}" }, footer]) },
  { id: "builtin:letter", name: "Letter", description: "A personal note with a signature.", kind: "builtin",
    document: layoutDocument("builtin:letter", [logo, { id: "body", type: "text", text: "Dear reader, a short note from all of us at Acme." }, footer]) },
  { id: "builtin:article", name: "Article", description: "A banner, a headline, the story and a button.", kind: "builtin", document: ARTICLE },
  { id: "builtin:spotlight", name: "Spotlight", description: "One offer, front and centre.", kind: "builtin",
    document: layoutDocument("builtin:spotlight", [logo, { id: "offer", type: "highlight", text: "Members save 20 percent" }, { id: "cta", type: "button", label: "Shop now", href: "https://acme.example" }, footer]) },
];
const ACME_LAYOUTS = [
  { id: "org:acme-monthly", name: "Monthly", description: "Our monthly letter.", kind: "organization", document: layoutDocument("org:acme-monthly", [logo, { id: "body", type: "text", text: "This month at Acme." }, footer]) },
  { id: "org:acme-welcome", name: "Welcome", description: "The first email a new member gets.", kind: "organization", document: layoutDocument("org:acme-welcome", [logo, { id: "headline", type: "heading", text: "Welcome" }, footer]) },
];
const acme = { id: "acme", name: "Acme Co" };
const client = { id: "client-org", name: "Client Example" };
const setup = (extra: Record<string, unknown> = {}) => ({ type: "setup", connected: true, domain: "acme.example", organizations: [acme], organizationId: "acme", layouts: BUILTINS, dictation: false, ...extra });
const rendered = `<html><head><style>body{margin:0}</style></head><body style="background-color:#eef0f3"><table><tr><td data-block="headline">Hello</td></tr><tr><td data-block="cta"><a href="https://acme.example/guide" onclick="alert(1)">Read the guide</a></td></tr></table><script>alert(1)</script></body></html>`;
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
  assert.deepEqual(f.posted("shown").map(m => m.state), [C.loading, C.chooseLayout, C.nameIt, C.ready]);
  f.dom.window.close();
});

test("Write it posts the whole request and the page shows the layout's shape while writing", () => {
  const f = ready();
  f.act(C.write).click();
  f.act(C.write).click();
  assert.deepEqual(f.posted("write"), [{ type: "write", layoutId: "builtin:article", name: "Autumn welcome", description }], "a double click writes once");
  assert.equal(f.doc.activeElement, f.el("bar-state"), "the focus waits on the state, not on an act that may go");
  f.send({ type: "writing", layoutId: "builtin:article" });
  assert.equal(f.visible("form"), false);
  assert.equal(f.visible("writing"), true);
  assert.equal(f.el("writing").querySelector(".sk-email")?.outerHTML, skeletonHTML(ARTICLE.blocks), "the chosen layout's shape");
  assert.ok(f.el("writing").closest(".loading"), "the shape sweeps while the email is written");
  assert.equal(f.barState(), C.writing);
  assert.ok(f.el("bar-state").classList.contains("busy"));
  assert.deepEqual(f.acts(), [[C.cancel, "text"]]);
  f.act(C.cancel).click();
  f.act(C.cancel).click();
  assert.deepEqual(f.posted("cancel"), [{ type: "cancel" }]);
  f.send({ type: "idle" });
  assert.equal(f.visible("form"), true);
  assert.equal(f.visible("writing"), false);
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
  f.send({ type: "writing", layoutId: "builtin:article" });
  f.send({ type: "failed", problem });
  assert.equal(f.visible("form"), true);
  assert.equal(f.visible("writing"), false);
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

test("thumbnails replace each card's skeleton as they arrive", () => {
  const f = fixture();
  Object.assign(globalThis, { DOMParser: f.dom.window.DOMParser });
  f.send(setup());
  for (const card of f.cards()) {
    assert.ok(card.querySelector(".sk-email"), "a skeleton until the render arrives");
    assert.equal(card.querySelector("iframe"), null);
    assert.ok(card.querySelector(".sk-email")!.closest('[aria-hidden="true"]'), "the picture is not read out: the card's name is");
  }
  assert.equal(f.card("builtin:article").querySelector(".sk-email")!.outerHTML, skeletonHTML(ARTICLE.blocks), "each layout's own shape");
  f.send({ type: "thumbnail", id: "builtin:article", html: rendered });
  const card = f.card("builtin:article"), skeleton = card.querySelector(".sk-email")!;
  const frame = card.querySelector("iframe")!;
  assert.ok(frame, "the render goes into a frame");
  assert.equal(frame.getAttribute("sandbox"), "", "a sandbox with no tokens: no scripts, no same origin");
  assert.equal(frame.getAttribute("tabindex"), "-1");
  assert.ok(frame.closest('[aria-hidden="true"]'));
  const srcdoc = frame.getAttribute("srcdoc")!;
  assert.equal(srcdoc, previewHTML(rendered, { blocks: false }));
  assert.doesNotMatch(srcdoc, /<script|href=|onclick|data-block/);
  assert.equal(f.visible(skeleton), true, "the skeleton stays until the render has loaded");
  assert.equal(f.visible(frame), false);
  f.loadThumbnail(frame);
  assert.equal(f.visible(frame), true);
  assert.equal(f.visible(skeleton), false);
  for (const id of ["builtin:confirmation", "builtin:letter", "builtin:spotlight"]) assert.equal(f.visible(f.card(id).querySelector(".sk-email")!), true, id);
  // A thumbnail for a layout the page does not list is ignored.
  f.send({ type: "thumbnail", id: "builtin:elsewhere", html: rendered });
  f.send({ type: "thumbnail", id: 7, html: rendered });
  assert.equal(f.doc.querySelectorAll("#layouts iframe").length, 1);
  // A render that failed sends no HTML: the card keeps its skeleton.
  f.send({ type: "thumbnail", id: "builtin:letter", html: "" });
  assert.equal(f.card("builtin:letter").querySelector("iframe"), null);
  assert.equal(f.visible(f.card("builtin:letter").querySelector(".sk-email")!), true);
  // A newer render loads behind the one shown and takes its place once loaded.
  f.send({ type: "thumbnail", id: "builtin:article", html: rendered.replace("Hello", "Hello again") });
  const frames = [...card.querySelectorAll("iframe")];
  assert.equal(frames.length, 2);
  const newer = frames.find(item => item !== frame)!;
  assert.equal(f.visible(frame), true, "no blank while the newer one loads");
  f.loadThumbnail(newer);
  assert.deepEqual([...card.querySelectorAll("iframe")], [newer]);
  assert.equal(f.visible(newer), true);
  // Two renders in a row: the older one finishing first gives way to the newer, never the other way round.
  const spotlight = f.card("builtin:spotlight");
  f.send({ type: "thumbnail", id: "builtin:spotlight", html: rendered });
  f.send({ type: "thumbnail", id: "builtin:spotlight", html: rendered.replace("Hello", "Newest") });
  const [older, newest] = [...spotlight.querySelectorAll("iframe")];
  f.loadThumbnail(older!);
  assert.deepEqual([...spotlight.querySelectorAll("iframe")], [newest]);
  assert.equal(f.visible(spotlight.querySelector(".sk-email")!), true, "the shape stays until the newest render has loaded");
  f.loadThumbnail(newest!);
  assert.equal(f.visible(newest!), true);
  assert.match(newest!.getAttribute("srcdoc")!, /Newest/);
  // The same list again keeps the cards and what they show; a changed layout goes back to its shape until its render comes.
  f.send(setup());
  assert.equal(f.card("builtin:article"), card);
  assert.equal(card.querySelector("iframe"), newer);
  const changed = BUILTINS.map(layout => layout.id === "builtin:article" ? { ...layout, document: layoutDocument("builtin:article", [logo, footer]) } : layout);
  f.send(setup({ layouts: changed }));
  assert.equal(f.card("builtin:article").querySelector("iframe"), null);
  assert.equal(f.card("builtin:article").querySelector(".sk-email")!.outerHTML, skeletonHTML([logo, footer]));
  f.dom.window.close();
});

test("the layout cards keep their size: 168 pixel columns from the left, each render shown at 168 by 218", () => {
  const f = fixture();
  f.send(setup({ layouts: [...ACME_LAYOUTS, ...BUILTINS] }));
  const style = (element: Element) => f.dom.window.getComputedStyle(element);
  const flat = (value: string) => value.replace(/\s+/g, "");
  // The organization's cards, the built-in ones and the shapes that stand in while they load: the same fixed tracks, never stretched.
  for (const id of ["cards-organization", "cards-builtin", "placeholders"]) {
    const grid = style(f.el(id));
    assert.equal(grid.display, "grid", id);
    assert.equal(flat(grid.getPropertyValue("grid-template-columns")), "repeat(auto-fill,168px)", id);
    assert.equal(grid.getPropertyValue("gap"), "16px", id);
    assert.equal(grid.getPropertyValue("justify-content"), "start", `${id} is left aligned`);
  }
  // A render is 600 by 780 pixels, shown at 0.28: exactly 168 by 218, the same for every card at every width.
  const card = f.card("builtin:article");
  const thumb = style(card.querySelector(".thumb")!), canvas = style(card.querySelector(".canvas")!);
  assert.deepEqual([thumb.width, thumb.height], ["168px", "218px"]);
  assert.deepEqual([canvas.width, canvas.height], ["600px", "780px"]);
  assert.match(flat(canvas.getPropertyValue("transform")), /^scale\(0?\.28\)$/);
  const tile = style(f.el("placeholders").querySelector(".tile")!);
  assert.deepEqual([tile.width, tile.height], ["168px", "218px"], "the loading shape is a card's size too");
  f.dom.window.close();
});

test("references show as chips; pictures and ZIPs have a switch; removing posts the index", () => {
  const f = fixture();
  f.send(setup());
  assert.equal(f.el("attach").getAttribute("aria-label"), C.addReferences);
  f.el("attach").click();
  assert.deepEqual(f.messages.at(-1), { type: "attach" });
  f.send({ type: "references", items: [
    { label: "brief.md", kind: "file", include: false, uploading: false },
    { label: "logo.png", kind: "picture", include: false, uploading: false },
    { label: "brand.zip", kind: "bundle", include: true, uploading: false },
    { label: "photo.jpg", kind: "picture", include: false, uploading: true },
  ] });
  const chips = () => [...f.el("references").querySelectorAll<HTMLElement>(".chip")];
  assert.deepEqual(chips().map(chip => chip.querySelector(".chip-name")?.textContent), ["brief.md", "logo.png", "brand.zip", "photo.jpg"]);
  const switchOf = (chip: HTMLElement) => chip.querySelector<HTMLInputElement>('input[type="checkbox"][role="switch"]');
  assert.equal(switchOf(chips()[0]!), null, "a file is a reference only");
  assert.equal(switchOf(chips()[1]!)!.checked, false, "off until the person turns it on");
  assert.equal(switchOf(chips()[2]!)!.checked, true);
  assert.equal(switchOf(chips()[1]!)!.closest("label")?.textContent, C.useInEmail);
  assert.ok(chips()[3]!.querySelector(".spinner"), "an uploading reference shows a spinner");
  assert.equal(chips()[0]!.querySelector(".spinner"), null);
  switchOf(chips()[1]!)!.click();
  assert.deepEqual(f.messages.at(-1), { type: "include", index: 1, on: true });
  switchOf(chips()[2]!)!.click();
  assert.deepEqual(f.messages.at(-1), { type: "include", index: 2, on: false });
  const remove = chips()[2]!.querySelector<HTMLButtonElement>("button.remove")!;
  assert.equal(remove.getAttribute("aria-label"), fill(C.removeReference, { name: "brand.zip" }));
  remove.click();
  assert.deepEqual(f.messages.at(-1), { type: "removeReference", index: 2 });
  // A label is text, never markup; a problem shows under the references until a list comes without one.
  const hostile = "<img src=x onerror=alert(1)>.png";
  f.send({ type: "references", items: [{ label: hostile, kind: "picture", include: false, uploading: false }, { label: 5, kind: "file" }], problem: { message: EMAIL_COPY.host.tooMany } });
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
  f.send(setup({ organizations: [acme, client], layouts: [...ACME_LAYOUTS, ...BUILTINS] }));
  assert.equal(f.visible("organization-row"), true);
  assert.equal(f.visible("meta"), false);
  const select = f.el("organization") as HTMLSelectElement;
  assert.equal(f.doc.querySelector('label[for="organization"]')?.textContent, C.organization);
  assert.deepEqual([...select.options].map(option => [option.value, option.textContent]), [["acme", "Acme Co"], ["client-org", "Client Example"]]);
  assert.equal(select.value, "acme");
  // Two groups of layouts: the organization's, under its name, then the built-in ones.
  const groups = () => [...f.el("layouts").querySelectorAll<HTMLElement>('[role="group"]')];
  assert.deepEqual(groups().map(group => f.named(group)), ["Acme Co", C.builtIn]);
  assert.deepEqual(groups().map(group => [...group.querySelectorAll<HTMLElement>('[role="radio"]')].map(card => card.dataset.id)), [ACME_LAYOUTS.map(layout => layout.id), BUILTINS.map(layout => layout.id)]);
  f.card("org:acme-welcome").click();
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
  assert.equal(groups().length, 0, "subheads only when both groups exist");
  assert.equal(f.cards().length, 4);
  assert.equal(f.cards().some(card => card.getAttribute("aria-checked") === "true"), false, "Acme's layout is not the client's");
  assert.equal(f.barState(), C.chooseLayout);
  // The choice went with Acme's layouts: back on Acme, nothing is chosen until the person chooses again.
  f.send(setup({ organizations: [acme, client], organizationId: "acme", layouts: [...ACME_LAYOUTS, ...BUILTINS] }));
  assert.equal(f.cards().some(card => card.getAttribute("aria-checked") === "true"), false);
  f.type("name", "Autumn welcome");
  assert.deepEqual(f.posted("draft").at(-1), { type: "draft", draft: { organizationId: "acme", name: "Autumn welcome" } });
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
  assert.equal(f.visible("writing"), false);
  assert.equal(f.visible("no-organization"), false);
  f.act(C.connect).click();
  f.act(C.connect).click();
  assert.deepEqual(f.posted("connect"), [{ type: "connect" }], "a double click connects once");
  // Not connected comes first, even mid-write.
  f.send({ type: "writing", layoutId: "builtin:article" });
  assert.deepEqual(f.acts(), [[C.connect, "button"]]);
  f.send({ type: "idle" });
  f.send(setup());
  assert.equal(f.barState(), C.chooseLayout);
  assert.equal(f.visible("form"), true);
  f.dom.window.close();
});

test("arrow keys move the layout choice", () => {
  const f = fixture();
  f.send(setup({ layouts: [...ACME_LAYOUTS, ...BUILTINS] }));
  const ids = [...ACME_LAYOUTS, ...BUILTINS].map(layout => layout.id);
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
  f.send({ type: "restore", draft: { organizationId: "client-org", layoutId: "org:client-news", name: "News" } });
  f.send(setup({ organizations: [acme, client], organizationId: "acme", layouts: [...ACME_LAYOUTS, ...BUILTINS] }));
  assert.deepEqual(f.posted("organization"), [{ type: "organization", id: "client-org" }]);
  assert.equal(f.barState(), C.loading);
  assert.equal((f.el("organization") as HTMLSelectElement).value, "client-org");
  const news = { id: "org:client-news", name: "News", description: "The client’s news.", kind: "organization", document: layoutDocument("org:client-news", [logo, footer]) };
  f.send(setup({ organizations: [acme, client], organizationId: "client-org", layouts: [news, ...BUILTINS] }));
  assert.equal(f.card("org:client-news").getAttribute("aria-checked"), "true");
  assert.equal(f.barState(), C.ready);
  // The host has the last word: a later list for another organization is not argued with.
  f.send(setup({ organizations: [acme, client], organizationId: "acme", layouts: [...ACME_LAYOUTS, ...BUILTINS] }));
  assert.equal(f.posted("organization").length, 1);
  assert.equal(f.barState(), C.chooseLayout);
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
  assert.deepEqual(f.messages.slice(-2), [{ type: "dictationCancel" }, { type: "write", layoutId: "builtin:article", name: "Autumn welcome", description: "Hello welcome everyone. and more" }]);
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
  forged({ type: "references", items: [{ label: "x.png", kind: "picture", include: false, uploading: false }] });
  forged({ type: "writing", layoutId: "builtin:article" });
  forged({ type: "layoutsFailed", problem });
  assert.equal(f.cards().length, 0);
  assert.equal((f.el("name") as HTMLInputElement).value, "");
  assert.equal(f.el("references").children.length, 0);
  assert.equal(f.barState(), C.loading);
  assert.deepEqual(f.messages.map(m => m.type), ["ready", "shown"]);
  f.send(setup());
  forged({ type: "thumbnail", id: "builtin:article", html: rendered });
  assert.equal(f.doc.querySelectorAll("#layouts iframe").length, 0);
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
  f.send({ type: "writing", layoutId: "builtin:article" });
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
  for (const id of ["meta", "no-organization", "form", "organization-row", "organization", "layout-group", "layouts", "name", "name-notice", "description", "attach", "dictate", "references", "dictation-status", "describe-notice", "writing", "bar", "bar-state", "bar-acts", "bar-notice"]) {
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

test("no host update takes focus while the window is elsewhere, and the page keeps the focus it holds", () => {
  const f = fixture();
  f.send(setup({ layouts: [...ACME_LAYOUTS, ...BUILTINS], dictation: true }));
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
  f.send({ type: "references", items: [{ label: "a.md", kind: "file", include: false, uploading: false }, { label: "b.md", kind: "file", include: false, uploading: false }] });
  f.el("references").querySelector<HTMLElement>(".chip button")!.focus();
  unfocused("a reference", () => f.send({ type: "references", items: [{ label: "b.md", kind: "file", include: false, uploading: false }] }));
  // A focused field, hidden by a write the host resumed.
  f.el("description").focus();
  unfocused("the description", () => f.send({ type: "writing", layoutId: "builtin:article" }));
  unfocused("the form coming back", () => f.send({ type: "idle" }));
  // While the page holds the focus, a card drawn again keeps it, and so does a reference.
  f.card("builtin:article").focus();
  f.send(setup({ layouts: [...ACME_LAYOUTS, ...BUILTINS], dictation: true }));
  assert.equal(f.doc.activeElement, f.card("builtin:article"));
  f.send({ type: "references", items: [{ label: "a.md", kind: "file", include: false, uploading: false }, { label: "b.md", kind: "file", include: false, uploading: false }] });
  const removeFirst = f.el("references").querySelector<HTMLElement>(".chip button")!;
  removeFirst.focus();
  removeFirst.click();
  f.send({ type: "references", items: [{ label: "b.md", kind: "file", include: false, uploading: false }] });
  assert.equal(f.doc.activeElement, f.el("references").querySelector(".chip button"), "the next reference takes the focus of the one removed");
  f.send({ type: "references", items: [] });
  assert.equal(f.doc.activeElement, f.el("attach"), "the last one gives it to Add references");
  // A field the page holds is hidden by a write: the focus goes to the state.
  f.card("builtin:article").click();
  f.type("name", "Autumn welcome");
  f.el("name").focus();
  f.send({ type: "writing", layoutId: "builtin:article" });
  assert.equal(f.doc.activeElement, f.el("bar-state"));
  f.dom.window.close();
});
