// LiveView (src/webview/ui/liveView.ts): a document once per screen, and
// messages for everything after.
//
// Driven against a recorder standing in for the webview, which is all the
// class touches: every case below is a claim about WHICH of the two calls a
// render makes, and what it carries.

import test from "node:test";
import assert from "node:assert/strict";

import { LiveView, type LogLine, type RegionParts } from "../src/webview/ui/liveView.js";
import { pageMessage } from "../src/webview/ui/protocol.js";
import { PAGE_LOG_LIMIT } from "../src/webview/ui/runtime.js";

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

test("before ready, the same screen is remembered, not re-assigned", () => {
  // Re-assigning on every render would reload a page that never gets far
  // enough to post ready -- an install renders several times a second.
  const r = recorder();
  r.view.render("running", PARTS);
  r.view.render("running", { ...PARTS, body: "B2" });
  r.view.render("running", { ...PARTS, body: "B3" });
  assert.equal(r.html.length, 1);
  assert.deepEqual(r.posts, []);

  assert.equal(r.view.handleMessage({ type: "ready" }), true);
  assert.deepEqual(r.posts, [{ type: "patch", regions: { body: "B3" } }], "ready brings the page up to the latest render");
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
  assert.equal(r.posts.length, before, "not ready yet: remembered only");
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
