// LiveView (src/webview/ui/liveView.ts): a document once per screen, and
// messages for everything after.
//
// Driven against a recorder standing in for the webview, which is all the
// class touches: every case below is a claim about WHICH of the two calls a
// render makes, and what it carries.

import test from "node:test";
import assert from "node:assert/strict";

import { LiveView, type LogLine, type RegionParts } from "../src/webview/ui/liveView.js";
import { pageDocument } from "../src/webview/ui/document.js";
import { pageMessage } from "../src/webview/ui/protocol.js";
import { PAGE_LOG_LIMIT } from "../src/webview/ui/runtime.js";
import { recorded, resetRecorded, window } from "./support/vscodeStub.js";

interface Recorder {
  html: string[];
  posts: Record<string, unknown>[];
  view: LiveView;
}

function recorder(options?: { maxLogLines?: number }): Recorder {
  const html: string[] = [];
  const posts: Record<string, unknown>[] = [];
  const view = new LiveView(
    {
      setHtml: (doc) => html.push(doc),
      postMessage: (msg) => posts.push(msg as Record<string, unknown>),
    },
    (parts, screen) => `<doc screen="${screen}">${parts.head}|${parts.body}|${parts.actions}</doc>`,
    options,
  );
  return { html, posts, view };
}

const PARTS: RegionParts = { head: "H1", body: "B1", actions: "A1" };

const line = (n: number): LogLine => ({ label: "Creating the cluster", text: `line ${n}` });

test("the first render assigns the whole document", () => {
  const r = recorder();
  r.view.render("running", PARTS);
  assert.deepEqual(r.html, [`<doc screen="running">H1|B1|A1</doc>`]);
  assert.deepEqual(r.posts, []);
});

test("before ready, a changed render of the same screen re-assigns the document", () => {
  // A patch posted before ready has nothing to land on. Re-assigning is what
  // keeps a page whose script never ran current, and what lets a panel test
  // that never sends ready keep reading every render from webview.html.
  const r = recorder();
  r.view.render("running", PARTS);
  r.view.render("running", { ...PARTS, body: "B2" });
  r.view.render("running", { ...PARTS, body: "B3" });
  assert.deepEqual(r.html, [
    `<doc screen="running">H1|B1|A1</doc>`,
    `<doc screen="running">H1|B2|A1</doc>`,
    `<doc screen="running">H1|B3|A1</doc>`,
  ]);
  assert.deepEqual(r.posts, []);

  r.view.render("running", { ...PARTS, body: "B3" });
  assert.equal(r.html.length, 3, "an unchanged render assigns nothing");

  assert.equal(r.view.handleMessage({ type: "ready" }), true);
  assert.deepEqual(r.posts, [], "the page already shows the latest render");
  r.view.render("running", { ...PARTS, body: "B4" });
  assert.equal(r.html.length, 3, "once ready, a change is a patch");
  assert.deepEqual(r.posts, [{ type: "patch", regions: { body: "B4" } }]);
});

test("after ready, the same screen posts only the regions that changed", () => {
  const r = recorder();
  r.view.render("running", PARTS);
  r.view.handleMessage({ type: "ready" });
  assert.deepEqual(r.posts, [], "nothing changed since the document was assigned");

  r.view.render("running", { ...PARTS, actions: "A2" });
  assert.deepEqual(r.posts, [{ type: "patch", regions: { actions: "A2" } }]);

  r.view.render("running", { head: "H3", body: "B3", actions: "A2" });
  assert.deepEqual(r.posts.at(-1), { type: "patch", regions: { head: "H3", body: "B3" } });

  const before = r.posts.length;
  r.view.render("running", { head: "H3", body: "B3", actions: "A2" });
  assert.equal(r.posts.length, before, "an identical render posts nothing");
  assert.equal(r.html.length, 1, "and none of it re-assigned the document");
});

test("a new screen assigns the document again, and waits for its ready", () => {
  const r = recorder();
  r.view.render("running", PARTS);
  r.view.handleMessage({ type: "ready" });
  r.view.render("failed", { ...PARTS, body: "failure" });
  assert.deepEqual(r.html.at(-1), `<doc screen="failed">H1|failure|A1</doc>`);
  assert.equal(r.html.length, 2);

  const before = r.posts.length;
  r.view.render("failed", { ...PARTS, body: "failure 2" });
  assert.equal(r.posts.length, before, "not ready yet: nothing is posted to a loading page");
  assert.equal(r.html.at(-1), `<doc screen="failed">H1|failure 2|A1</doc>`, "the new screen's document is re-assigned instead");
});

test("invalidate makes the next render assign the whole document", () => {
  const r = recorder();
  r.view.render("running", PARTS);
  r.view.handleMessage({ type: "ready" });
  r.view.invalidate();
  r.view.render("running", PARTS);
  assert.equal(r.html.length, 2, "a theme change restyles every rule, so the document is rebuilt");
});

test("a reloaded page is patched from the ASSIGNED document, not from the last patch", () => {
  // A hidden panel without retainContextWhenHidden is rebuilt from the html it
  // was assigned; every patch since is gone. Its ready must bring it all back.
  const r = recorder();
  r.view.render("running", PARTS);
  r.view.handleMessage({ type: "ready" });
  r.view.render("running", { ...PARTS, body: "B2" });
  r.view.render("running", { ...PARTS, body: "B2", actions: "A2" });
  r.posts.length = 0;

  r.view.handleMessage({ type: "ready" });
  assert.deepEqual(r.posts, [{ type: "patch", regions: { body: "B2", actions: "A2" } }]);
});

test("progress is posted once ready, and re-sent on every ready", () => {
  const r = recorder();
  r.view.render("running", PARTS);
  r.view.progress({ percent: 10, status: "Checking Docker", state: "running", startedAt: 1000 });
  assert.deepEqual(r.posts, [], "a loading page would drop it");

  r.view.handleMessage({ type: "ready" });
  assert.deepEqual(r.posts, [{ type: "progress", percent: 10, status: "Checking Docker", state: "running", startedAt: 1000 }]);

  r.view.progress({ percent: 40, status: "Creating the cluster", state: "running", startedAt: 1000 });
  assert.deepEqual(r.posts.at(-1), { type: "progress", percent: 40, status: "Creating the cluster", state: "running", startedAt: 1000 });

  r.posts.length = 0;
  r.view.handleMessage({ type: "ready" });
  assert.deepEqual(r.posts, [{ type: "progress", percent: 40, status: "Creating the cluster", state: "running", startedAt: 1000 }]);
});

test("log lines stream while ready, and the whole buffer is re-sent on ready", () => {
  const r = recorder();
  r.view.render("running", PARTS);
  r.view.log([line(1), line(2)]);
  assert.deepEqual(r.posts, []);

  r.view.handleMessage({ type: "ready" });
  assert.deepEqual(r.posts, [{ type: "log", lines: [line(1), line(2)], reset: true }]);

  r.view.log([line(3)]);
  assert.deepEqual(r.posts.at(-1), { type: "log", lines: [line(3)], reset: false }, "only the new line crosses the bridge");

  r.posts.length = 0;
  r.view.handleMessage({ type: "ready" });
  assert.deepEqual(r.posts, [{ type: "log", lines: [line(1), line(2), line(3)], reset: true }]);

  r.view.log([line(9)], { reset: true });
  assert.deepEqual(r.posts.at(-1), { type: "log", lines: [line(9)], reset: true });
  assert.deepEqual(r.view.logLines(), [line(9)]);
});

test("the log buffer is bounded, keeping the newest lines", () => {
  const small = recorder({ maxLogLines: 10 });
  small.view.render("running", PARTS);
  small.view.log(Array.from({ length: 25 }, (_, n) => line(n + 1)));
  assert.equal(small.view.logLines().length, 10);
  assert.deepEqual(small.view.logLines()[0], line(16));
  assert.deepEqual(small.view.logLines().at(-1), line(25));

  const standard = recorder();
  standard.view.render("running", PARTS);
  for (let n = 1; n <= PAGE_LOG_LIMIT + 1000; n += 1) standard.view.log([line(n)]);
  assert.equal(standard.view.logLines().length, PAGE_LOG_LIMIT, "the default bound is the page's own");
  assert.deepEqual(standard.view.logLines()[0], line(1001));

  standard.view.handleMessage({ type: "ready" });
  const sent = standard.posts.at(-1) as { lines: LogLine[] };
  assert.equal(sent.lines.length, PAGE_LOG_LIMIT, "a reloaded page gets at most the bound");
});

test("handleMessage consumes ready and nothing else", () => {
  const r = recorder();
  assert.equal(r.view.handleMessage({ type: "install" }), false);
  assert.equal(r.view.handleMessage(null), false);
  assert.equal(r.view.handleMessage("ready"), false);
  assert.equal(r.view.handleMessage({ type: "ready" }), true);
});

test("pageMessage accepts a typed message and nothing else", () => {
  assert.deepEqual(pageMessage({ type: "input", field: "name", value: "x" }), { type: "input", field: "name", value: "x" });
  assert.equal(pageMessage({ type: "" }), undefined);
  assert.equal(pageMessage({ value: "x" }), undefined);
  assert.equal(pageMessage(null), undefined);
  assert.equal(pageMessage("ready"), undefined);
});

test("through a stub panel: html until ready, then patches, progress and log on posted", () => {
  // The adoption path a panel takes, end to end against the test stub the
  // panel tests use: the two-line target adapter, a real pageDocument wrap,
  // and the page's side played by send(). A panel test that never sends ready
  // reads every render from html; one that does reads the rest from posted.
  resetRecorded();
  const surface = window.createWebviewPanel("memql.probe", "Probe");
  const panel = recorded.webviews.at(-1);
  assert.ok(panel !== undefined);
  const view = new LiveView(
    { setHtml: (h) => (surface.webview.html = h), postMessage: (m) => surface.webview.postMessage(m) },
    (parts, screen) => pageDocument({ nonce: "n0nce", title: "Probe", themeAttr: "", screen, ...parts }),
  );
  surface.webview.onDidReceiveMessage((raw) => view.handleMessage(raw));

  view.render("run", { head: "<h1>One</h1>", body: "", actions: "" });
  view.render("run", { head: "<h1>Two</h1>", body: "", actions: "" });
  assert.equal(panel.renders, 2);
  assert.ok(panel.html.includes("<h1>Two</h1>"), "before ready, html is the latest render");
  view.progress({ percent: 30, status: "Creating the cluster", state: "running" });
  view.log([line(1)]);
  assert.deepEqual(panel.posted, [], "nothing is posted to a page that is still loading");

  panel.send({ type: "ready" });
  assert.deepEqual(panel.posted, [
    { type: "progress", percent: 30, status: "Creating the cluster", state: "running" },
    { type: "log", lines: [line(1)], reset: true },
  ]);
  view.render("run", { head: "<h1>Two</h1>", body: "<p>Body</p>", actions: "" });
  assert.equal(panel.renders, 2, "a ready page is patched, not reloaded");
  assert.deepEqual(panel.posted.at(-1), { type: "patch", regions: { body: "<p>Body</p>" } });
  panel.close();
});
