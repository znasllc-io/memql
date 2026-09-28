// The one page script (src/webview/ui/runtime.ts), as far as it can be run
// without a browser.
//
// There is no DOM under `node --test`, so this file does two things. It pins
// the properties of the STRING -- no backtick, no dollar-brace, no backslash,
// and it parses -- because the script is embedded in template literals where
// the first two would be consumed before a browser ever saw them. And it runs
// the script against a small hand-made document that records listeners, which
// is enough to check the page-to-host half of the protocol: `ready` on load,
// the click message with its data attributes, fields, switches and Escape.
// The host-to-page half (patch, progress, log) moves real DOM, so it is
// checked in headless Chrome instead: `npm run gallery:check` runs
// gallery/checks/runtime.checks.js against a real kit page and fails on any
// check that does not hold. Run it after changing runtime.ts.

import test from "node:test";
import assert from "node:assert/strict";

import { PAGE_LOG_LIMIT, PAGE_RUNTIME } from "../src/webview/ui/runtime.js";

test("the runtime can be embedded in a template literal", () => {
  assert.ok(!PAGE_RUNTIME.includes("`"), "no backtick");
  assert.ok(!PAGE_RUNTIME.includes("${"), "no dollar-brace");
  assert.ok(!PAGE_RUNTIME.includes("\\"), "no backslash: a template literal would reinterpret it");
  // It is also inlined in a <script> element, which the first of these would
  // end early and the second would turn into an HTML comment.
  assert.ok(!/<\/script/i.test(PAGE_RUNTIME), "no closing script tag");
  assert.ok(!PAGE_RUNTIME.includes("<!--"), "no comment opener");
  assert.ok(PAGE_RUNTIME.includes(`var LOG_LIMIT = ${PAGE_LOG_LIMIT};`), "the page keeps as many lines as LiveView buffers");
});

test("the runtime parses", () => {
  assert.doesNotThrow(() => new Function(PAGE_RUNTIME));
});

// -----------------------------------------------------------------------------
// A document just large enough to run the script
// -----------------------------------------------------------------------------

type Listener = (event: unknown) => void;

class FakeElement {
  dataset: Record<string, string>;
  disabled = false;
  checked = false;
  value = "";
  type = "";
  id = "";
  tagName = "BUTTON";
  private readonly attrs: Map<string, string>;
  constructor(attrs: Record<string, string>, private readonly act: FakeElement | null = null) {
    this.attrs = new Map(Object.entries(attrs));
    this.dataset = {};
    for (const [name, value] of this.attrs) {
      if (name.startsWith("data-")) {
        this.dataset[name.slice(5).replace(/-([a-z])/g, (_m, c: string) => c.toUpperCase())] = value;
      }
    }
    this.id = attrs.id ?? "";
  }
  closest(selector: string): FakeElement | null {
    return selector === "[data-act]" ? (this.act ?? (this.attrs.has("data-act") ? this : null)) : null;
  }
  hasAttribute(name: string): boolean {
    return this.attrs.has(name);
  }
  getAttribute(name: string): string | null {
    return this.attrs.get(name) ?? null;
  }
  setAttribute(name: string, value: string): void {
    this.attrs.set(name, value);
  }
  matches(): boolean {
    return false;
  }
}

interface Harness {
  posted: Record<string, unknown>[];
  documentListeners: Map<string, Listener>;
  windowListeners: Map<string, Listener>;
  body: FakeElement;
}

function runPage(bodyAttrs: Record<string, string> = {}): Harness {
  const posted: Record<string, unknown>[] = [];
  const documentListeners = new Map<string, Listener>();
  const windowListeners = new Map<string, Listener>();
  const body = new FakeElement(bodyAttrs);
  const document = {
    body,
    activeElement: body,
    addEventListener: (type: string, fn: Listener) => documentListeners.set(type, fn),
    querySelector: () => null,
    querySelectorAll: () => [],
    getElementById: () => null,
  };
  const window = {
    scrollY: 0,
    addEventListener: (type: string, fn: Listener) => windowListeners.set(type, fn),
    scrollTo: () => undefined,
  } as Record<string, unknown>;
  const vscode = {
    postMessage: (msg: Record<string, unknown>) => posted.push(msg),
    getState: () => undefined,
    setState: () => undefined,
  };
  let acquired = 0;
  const acquire = (): typeof vscode => {
    acquired += 1;
    return vscode;
  };
  const run = new Function("acquireVsCodeApi", "document", "window", "Element", "HTMLElement", "CSS", PAGE_RUNTIME);
  run(acquire, document, window, FakeElement, FakeElement, { escape: (s: string) => s });
  assert.equal(acquired, 1, "the VS Code API is acquired exactly once");
  assert.equal(typeof (window.memqlPage as { post?: unknown } | undefined)?.post, "function", "panel scripts can post through the page");
  return { posted, documentListeners, windowListeners, body };
}

test("the page posts ready once it has loaded", () => {
  const page = runPage();
  assert.deepEqual(page.posted, [{ type: "ready" }]);
  assert.ok(page.windowListeners.has("message"), "it listens for the host");
});

test("a click posts the nearest data-act with its value and every other data attribute", () => {
  const page = runPage();
  const button = new FakeElement({ "data-act": "choose", "data-value": "k3d", "data-step-id": "clusterUp" });
  const icon = new FakeElement({}, button);
  page.documentListeners.get("click")?.({ target: icon, preventDefault: () => undefined });
  assert.deepEqual(page.posted.at(-1), { type: "choose", value: "k3d", stepId: "clusterUp" });
});

test("a click on a disabled or busy act posts nothing", () => {
  const page = runPage();
  const disabled = new FakeElement({ "data-act": "install" });
  disabled.disabled = true;
  const busy = new FakeElement({ "data-act": "signIn", "aria-disabled": "true" });
  page.documentListeners.get("click")?.({ target: disabled, preventDefault: () => undefined });
  page.documentListeners.get("click")?.({ target: busy, preventDefault: () => undefined });
  assert.deepEqual(page.posted, [{ type: "ready" }]);
});

test("a disclosure toggle flips locally and tells the host which way", () => {
  const page = runPage();
  const toggle = new FakeElement({ "data-act": "toggleLogs", "data-disclosure": "run-logs", "aria-expanded": "false", "aria-controls": "run-logs" });
  page.documentListeners.get("click")?.({ target: toggle, preventDefault: () => undefined });
  assert.deepEqual(page.posted.at(-1), { type: "toggleLogs", disclosure: "run-logs", open: true });
  assert.equal(toggle.getAttribute("aria-expanded"), "true");
  page.documentListeners.get("click")?.({ target: toggle, preventDefault: () => undefined });
  assert.deepEqual(page.posted.at(-1), { type: "toggleLogs", disclosure: "run-logs", open: false });
});

test("a field posts its value on input, once per distinct value, and a checkbox field posts true/false", () => {
  const page = runPage();
  const input = new FakeElement({ "data-field": "address" });
  input.value = "https://api.memql.localhost";
  page.documentListeners.get("input")?.({ type: "input", target: input });
  page.documentListeners.get("change")?.({ type: "change", target: input });
  assert.deepEqual(page.posted.slice(1), [{ type: "input", field: "address", value: "https://api.memql.localhost" }]);

  const box = new FakeElement({ "data-field": "shared" });
  box.type = "checkbox";
  box.checked = true;
  page.documentListeners.get("change")?.({ type: "change", target: box });
  assert.deepEqual(page.posted.at(-1), { type: "input", field: "shared", value: "true" });
});

test("a switch posts its act, id, state and data on change", () => {
  const page = runPage();
  const sw = new FakeElement({ role: "switch", id: "s-k3d", "data-switch-act": "shared", "data-value": "k3d" });
  sw.type = "checkbox";
  sw.checked = true;
  page.documentListeners.get("change")?.({ type: "change", target: sw });
  assert.deepEqual(page.posted.at(-1), { type: "shared", value: "k3d", id: "s-k3d", checked: true });

  const plain = new FakeElement({ role: "switch", id: "s-plain" });
  plain.type = "checkbox";
  page.documentListeners.get("change")?.({ type: "change", target: plain });
  assert.deepEqual(page.posted.at(-1), { type: "switch", id: "s-plain", checked: false });
});

test("Escape posts the body's escape act, and Enter posts a field's enter act", () => {
  const page = runPage({ "data-escape-act": "back" });
  page.documentListeners.get("keydown")?.({ key: "Escape", target: page.body, defaultPrevented: false, isComposing: false });
  assert.deepEqual(page.posted.at(-1), { type: "back" });

  const input = new FakeElement({ "data-field": "phrase", "data-enter-act": "confirm" });
  input.value = "delete memql data";
  page.documentListeners.get("keydown")?.({
    key: "Enter",
    target: input,
    defaultPrevented: false,
    isComposing: false,
    preventDefault: () => undefined,
  });
  assert.deepEqual(page.posted.at(-1), { type: "confirm", field: "phrase", value: "delete memql data" });
});

test("host messages with nothing to land on are harmless", () => {
  const page = runPage();
  const onMessage = page.windowListeners.get("message");
  assert.ok(onMessage);
  assert.doesNotThrow(() => {
    onMessage({ data: { type: "patch", regions: { body: "<p>x</p>" } } });
    onMessage({ data: { type: "progress", percent: 40, status: "Creating the cluster", state: "running" } });
    onMessage({ data: { type: "log", lines: [{ text: "x" }], reset: true } });
    onMessage({ data: { type: "setDisclosure", id: "run-logs", open: true } });
    onMessage({ data: null });
    onMessage({ data: "not an object" });
  });
});
