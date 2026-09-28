// The page kit (src/webview/ui/kit.ts): what every renderer promises.
//
// Four claims, each mechanically checkable:
//   1. Every TEXT argument is escaped inside the kit, and every `*Html`
//      argument is interpolated verbatim -- the naming is the whole contract,
//      so both halves are pinned.
//   2. The action bar enforces its own rules: at most three acts, at most one
//      button, the button last.
//   3. The accessible structure is there: a switch is a native checkbox with
//      role=switch and a label; the progress bar is a progressbar with a value
//      (or none, when indeterminate); loading is a status.
//   4. Nothing the kit writes carries a `style` attribute, which this CSP
//      would drop in silence.

import test from "node:test";
import assert from "node:assert/strict";

import { escapeHtml } from "@znasllc-io/memql-view-kit";

import { brandStyleBlock } from "../src/webview/brandTokens.js";
import { pageDocument } from "../src/webview/ui/document.js";
import {
  actionBar,
  button,
  clampPercent,
  codeBlock,
  disclosure,
  emptyState,
  facts,
  field,
  formatElapsed,
  head,
  kitStyles,
  logPane,
  notice,
  page,
  progress,
  skeleton,
  subhead,
  switchRow,
  textInput,
  type Act,
} from "../src/webview/ui/kit.js";
import { PAGE_RUNTIME } from "../src/webview/ui/runtime.js";

/** A distinct hostile string per argument, so a failure names the argument that leaked. */
function hostile(name: string): string {
  return `<img src=x onerror="${name}">'&${name}`;
}

/** Each named argument arrived escaped, and none arrived raw. */
function assertEscaped(html: string, names: readonly string[], where: string): void {
  for (const name of names) {
    const raw = hostile(name);
    assert.ok(!html.includes(raw), `${where}: ${name} reached the page unescaped`);
    assert.ok(html.includes(escapeHtml(raw)), `${where}: ${name} is missing from the page`);
  }
  assert.ok(!html.includes("<img"), `${where}: a raw tag survived`);
}

const act = (name: string, over: Partial<Act> = {}): Act => ({ act: hostile(`${name}.act`), label: hostile(`${name}.label`), ...over });

// -----------------------------------------------------------------------------
// 1. Escaping
// -----------------------------------------------------------------------------

test("button escapes its label, act, value, title and data values", () => {
  const html = button({
    act: hostile("act"),
    label: hostile("label"),
    value: hostile("value"),
    title: hostile("title"),
    data: { step: hostile("data") },
  });
  assertEscaped(html, ["act", "label", "value", "title", "data"], "button");
});

test("head escapes its title, meta, back and aside acts", () => {
  const html = head({
    title: hostile("title"),
    meta: hostile("meta"),
    back: { act: hostile("back.act"), label: hostile("back.label"), value: hostile("back.value") },
    asideActs: [act("aside")],
  });
  assertEscaped(html, ["title", "meta", "back.act", "back.label", "back.value", "aside.act", "aside.label"], "head");
});

test("subhead and facts escape every text argument", () => {
  assertEscaped(subhead(hostile("title"), hostile("meta")), ["title", "meta"], "subhead");
  assertEscaped(
    facts([
      { label: hostile("label1"), value: hostile("value1"), mono: true },
      { label: hostile("label2"), value: hostile("value2"), muted: true },
    ]),
    ["label1", "value1", "label2", "value2"],
    "facts",
  );
});

test("actionBar escapes its state, detail, confirm and acts", () => {
  const html = actionBar({
    state: hostile("state"),
    detail: hostile("detail"),
    confirm: hostile("confirm"),
    acts: [act("a"), act("b", { tone: "primary" })],
  });
  assertEscaped(html, ["state", "detail", "confirm", "a.act", "a.label", "b.act", "b.label"], "actionBar");
});

test("field and textInput escape every text argument", () => {
  assertEscaped(
    field({ label: hostile("label"), hint: hostile("hint"), error: hostile("error"), id: hostile("id"), controlHtml: "" }),
    ["label", "hint", "error", "id"],
    "field with id",
  );
  assertEscaped(
    field({ label: hostile("label"), hint: hostile("hint"), controlHtml: "" }),
    ["label", "hint"],
    "field without id",
  );
  assertEscaped(
    textInput({
      field: hostile("field"),
      value: hostile("value"),
      placeholder: hostile("placeholder"),
      id: hostile("id"),
      describedBy: hostile("describedBy"),
      enterAct: hostile("enterAct"),
    }),
    ["field", "value", "placeholder", "id", "describedBy", "enterAct"],
    "textInput",
  );
});

test("switchRow escapes its id, label, note and data values", () => {
  assertEscaped(
    switchRow({ id: hostile("id"), label: hostile("label"), note: hostile("note"), checked: true, data: { value: hostile("data") } }),
    ["id", "label", "note", "data"],
    "switchRow",
  );
});

test("notice, codeBlock and emptyState escape every text argument", () => {
  assertEscaped(
    notice({ tone: "error", line: hostile("line"), next: hostile("next"), acts: [act("fix")] }),
    ["line", "next", "fix.act", "fix.label"],
    "notice",
  );
  assertEscaped(codeBlock({ text: hostile("text"), act: act("copy") }), ["text", "copy.act", "copy.label"], "codeBlock");
  assertEscaped(emptyState({ line: hostile("line"), acts: [act("add")] }), ["line", "add.act", "add.label"], "emptyState");
});

test("disclosure escapes its act, labels, meta and id", () => {
  assertEscaped(
    disclosure({
      act: hostile("act"),
      label: hostile("label"),
      openLabel: hostile("openLabel"),
      meta: hostile("meta"),
      id: hostile("id"),
      open: false,
      bodyHtml: "",
    }),
    ["act", "label", "openLabel", "meta", "id"],
    "disclosure",
  );
});

test("logPane escapes its id, label, empty text, acts and every line", () => {
  assertEscaped(
    logPane({
      id: hostile("id"),
      ariaLabel: hostile("ariaLabel"),
      empty: hostile("empty"),
      acts: [act("copy")],
      lines: [{ label: hostile("line.label"), text: hostile("line.text"), tone: "error" }],
    }),
    ["id", "ariaLabel", "empty", "copy.act", "copy.label", "line.label", "line.text"],
    "logPane",
  );
});

test("progress and skeleton escape every text argument", () => {
  assertEscaped(
    progress({ title: hostile("title"), status: hostile("status"), stepText: hostile("stepText"), id: hostile("id"), state: "running", percent: 10 }),
    ["title", "status", "stepText", "id"],
    "progress",
  );
  assertEscaped(skeleton({ shape: "page", label: hostile("label") }), ["label"], "skeleton");
});

test("*Html arguments are interpolated verbatim", () => {
  const marker = `<span class="caller-markup">kept</span>`;
  assert.ok(facts([{ label: "Status", valueHtml: marker }]).includes(marker), "facts valueHtml");
  assert.ok(field({ label: "Name", controlHtml: marker }).includes(marker), "field controlHtml");
  assert.ok(notice({ tone: "info", line: "x", codeHtml: marker }).includes(marker), "notice codeHtml");
  assert.ok(disclosure({ act: "t", label: "x", open: false, id: "d", bodyHtml: marker }).includes(marker), "disclosure bodyHtml");
  const p = page({ head: marker, body: marker, actionBar: marker });
  assert.equal(p.split(marker).length - 1, 3, "page interpolates head, body and actionBar");
});

test("a data-* key that is not a plain attribute name throws rather than becoming markup", () => {
  assert.throws(() => button({ act: "a", label: "A", data: { 'x" onclick="1': "v" } }), /not a usable data-\* attribute name/);
  assert.throws(() => button({ act: "a", label: "A", data: { Upper: "v" } }), /not a usable/);
  assert.throws(() => button({ act: "a", label: "A", data: { act: "other" } }), /written by the kit/);
  assert.match(button({ act: "a", label: "A", data: { "switch-act": "v" } }), / data-switch-act="v"/);
});

// -----------------------------------------------------------------------------
// 2. The action bar's rules
// -----------------------------------------------------------------------------

test("actionBar throws on a fourth act", () => {
  assert.throws(
    () =>
      actionBar({
        state: "Ready",
        acts: [
          { act: "a", label: "A" },
          { act: "b", label: "B" },
          { act: "c", label: "C" },
          { act: "d", label: "D", tone: "primary" },
        ],
      }),
    /at most three acts/,
  );
});

test("actionBar throws on a second button, primary or danger", () => {
  assert.throws(
    () =>
      actionBar({
        state: "Ready",
        acts: [
          { act: "a", label: "A", tone: "primary" },
          { act: "b", label: "B", tone: "primary" },
        ],
      }),
    /at most one button/,
  );
  assert.throws(
    () =>
      actionBar({
        state: "Ready",
        acts: [
          { act: "a", label: "Retry", tone: "primary" },
          { act: "b", label: "Uninstall", tone: "danger" },
        ],
      }),
    /at most one button/,
  );
});

test("actionBar draws one button, last, and every other act as text", () => {
  const html = actionBar({
    state: "Signed out",
    tone: "warn",
    acts: [
      { act: "signIn", label: "Sign in", tone: "primary" },
      { act: "code", label: "Use a code instead", tone: "secondary" },
      { act: "remove", label: "Remove from list" },
    ],
  });
  const acts = [...html.matchAll(/<button [^>]*class="(mq-btn|mq-textbtn)"[^>]*data-act="([^"]+)"/g)].map((m) => [m[1], m[2]]);
  assert.deepEqual(acts, [
    ["mq-textbtn", "code"],
    ["mq-textbtn", "remove"],
    ["mq-btn", "signIn"],
  ]);
  assert.match(html, /data-tone="warn"/);
  assert.match(html, /<span class="mq-actbar-word">Signed out<\/span>/);
});

test("actionBar with no acts renders the state alone", () => {
  const html = actionBar({ state: "Stopping", tone: "busy", acts: [] });
  assert.doesNotMatch(html, /mq-actbar-acts/);
  assert.match(html, /Stopping/);
});

// -----------------------------------------------------------------------------
// 3. Accessible structure
// -----------------------------------------------------------------------------

test("switchRow is a native checkbox with role=switch, its checked state and its label", () => {
  const on = switchRow({ id: "delete-data", label: "Delete the cluster's data", note: "Can't be undone.", checked: true, tone: "danger" });
  assert.match(on, /^<label class="mq-switch" for="delete-data" data-tone="danger">/);
  assert.match(on, /<input class="mq-switch-input" type="checkbox" role="switch" id="delete-data" checked/);
  assert.match(on, /aria-labelledby="delete-data-label"/);
  assert.match(on, /<span class="mq-switch-label" id="delete-data-label">Delete the cluster&#39;s data<\/span>/);
  assert.match(on, /aria-describedby="delete-data-note"/);
  assert.match(on, /<span class="mq-switch-note" id="delete-data-note">/);
  assert.match(on, /class="mq-switch-track" aria-hidden="true"/);

  const off = switchRow({ id: "k3d", label: "k3d", checked: false, disabled: true, data: { "switch-act": "shared", value: "k3d" } });
  assert.doesNotMatch(off, / checked/);
  assert.match(off, / disabled/);
  assert.doesNotMatch(off, /aria-describedby/, "no note, no description");
  assert.match(off, /data-switch-act="shared" data-value="k3d"/);
});

test("progress renders a progressbar with its value, and no style attribute", () => {
  const html = progress({ title: "Installing MemQL", percent: 40, status: "Creating the cluster", stepText: "Step 10 of 16", state: "running" });
  assert.match(html, /role="progressbar"/);
  assert.match(html, /aria-valuemin="0" aria-valuemax="100" aria-valuenow="40"/);
  assert.match(html, /aria-valuetext="40%, Creating the cluster"/);
  assert.match(html, /class="mq-bar-fill" data-part="fill" data-percent="40"/);
  assert.match(html, /role="status" aria-live="polite">Creating the cluster</);
  assert.match(html, /data-region="progress"/);
  assert.match(html, /<svg class="memql-mark"/, "the MemQL mark");
  assert.doesNotMatch(html, /\sstyle=/);
  assert.doesNotMatch(html, /data-indeterminate/);
});

test("progress without a percent is indeterminate and claims no value", () => {
  const html = progress({ title: "Installing MemQL", status: "Checking this computer", state: "running" });
  assert.match(html, /role="progressbar"/);
  assert.doesNotMatch(html, /aria-valuenow/);
  assert.doesNotMatch(html, /data-percent/);
  assert.match(html, /data-indeterminate="true"/);
  const nan = progress({ title: "t", status: "s", state: "running", percent: Number.NaN });
  assert.match(nan, /data-indeterminate="true"/, "NaN is not a percentage");
});

test("progress clamps and rounds its percent, and the meta line carries the clock", () => {
  assert.equal(clampPercent(140), 100);
  assert.equal(clampPercent(-5), 0);
  assert.equal(clampPercent(41.6), 42);
  assert.equal(clampPercent(undefined), undefined);

  const now = Date.UTC(2026, 8, 28, 10, 0, 0);
  const running = progress({ title: "t", status: "s", stepText: "Step 6 of 16", startedAt: now - 192_000, state: "running", now });
  assert.match(running, /<span data-part="step">Step 6 of 16<\/span><span data-part="sep" aria-hidden="true"> · <\/span><span data-part="elapsed">3:12<\/span>/);
  assert.match(running, new RegExp(`data-started-at="${now - 192_000}"`));

  const failed = progress({ title: "t", status: "s", startedAt: now - 60_000, endedAt: now - 30_000, state: "failed", now });
  assert.match(failed, /<span data-part="elapsed">0:30<\/span>/, "a settled run's clock is frozen at its true duration");
  assert.match(failed, /data-part="sep" aria-hidden="true" hidden/, "no separator without step text");

  assert.equal(formatElapsed(3_723_000), "1:02:03");
  assert.equal(formatElapsed(-10), "0:00");
});

test("disclosure emits its body always, hidden while closed, under aria-controls", () => {
  const closed = disclosure({ act: "toggleLogs", label: "Show logs", openLabel: "Hide logs", open: false, id: "run-logs", bodyHtml: "<p>x</p>" });
  assert.match(closed, /data-act="toggleLogs" data-disclosure="run-logs" aria-expanded="false" aria-controls="run-logs"/);
  assert.match(closed, /<div class="mq-disclosure-body" id="run-logs" hidden><p>x<\/p><\/div>/);
  assert.match(closed, /<span data-when="closed">Show logs<\/span><span data-when="open">Hide logs<\/span>/);
  const open = disclosure({ act: "toggleLogs", label: "Show logs", open: true, id: "run-logs", bodyHtml: "" });
  assert.match(open, /aria-expanded="true"/);
  assert.match(open, /<div class="mq-disclosure-body" id="run-logs">/);
});

test("logPane is the region the page script follows and appends to", () => {
  const html = logPane({ id: "run-log", ariaLabel: "Install log", lines: [{ label: "Checking Docker", text: "ok", tone: "muted" }] });
  assert.match(html, /id="run-log" data-region="log" data-follow="true" role="log" aria-live="off" aria-label="Install log" tabindex="0"/);
  assert.match(html, /<div class="mq-log-line" data-tone="muted"><span class="mq-log-label">Checking Docker<\/span><span class="mq-log-text">ok<\/span><\/div>/);
});

test("skeleton is a busy status whose words are for screen readers only", () => {
  for (const shape of ["facts", "list", "form", "page"] as const) {
    const html = skeleton({ shape, label: "Loading cluster details" });
    assert.match(html, /role="status" aria-busy="true"/, shape);
    assert.match(html, /<span class="mq-sr">Loading cluster details<\/span><div aria-hidden="true">/, shape);
    assert.match(html, /class="mq-skel"/, shape);
  }
});

test("field points its label at the control, or wraps it", () => {
  const withId = field({ id: "f-name", label: "Name", error: "Taken.", controlHtml: textInput({ id: "f-name", field: "name", invalid: true }) });
  assert.match(withId, /<label class="mq-field-label" for="f-name">Name<\/label><input class="mq-input" type="text" id="f-name" data-field="name"/);
  assert.match(withId, /aria-invalid="true"/);
  assert.match(withId, /<p class="mq-field-error" role="alert" id="f-name-error">Taken.<\/p>/);
  const wrapped = field({ label: "Name", controlHtml: "<input>" });
  assert.match(wrapped, /<label class="mq-field-main"><span class="mq-field-label">Name<\/span><input><\/label>/);
});

test("page is the three regions the runtime patches, with nothing between the footer tags", () => {
  const html = page({ head: "H", body: "B" });
  assert.equal(
    html,
    `<main class="mq-page"><div data-region="head">H</div><div data-region="body">B</div></main><footer data-region="actions"></footer>`,
  );
});

// -----------------------------------------------------------------------------
// 4. No inline styles, and the sheet rides every document
// -----------------------------------------------------------------------------

test("nothing the kit renders carries a style attribute", () => {
  const everything = [
    button({ act: "a", label: "A", tone: "primary", busy: true }),
    head({ title: "T", meta: "m", back: { act: "b", label: "B" }, asideActs: [{ act: "e", label: "Edit" }] }),
    subhead("S", "m"),
    facts([{ label: "L", value: "" }]),
    actionBar({ state: "S", tone: "busy", confirm: "C", acts: [{ act: "a", label: "A", tone: "danger" }] }),
    field({ label: "L", hint: "h", error: "e", id: "i", controlHtml: textInput({ field: "f" }) }),
    switchRow({ id: "s", label: "L", note: "n", checked: true }),
    notice({ tone: "warn", line: "l", next: "n", codeHtml: codeBlock({ text: "t" }), acts: [{ act: "a", label: "A" }] }),
    disclosure({ act: "t", label: "L", open: true, id: "d", bodyHtml: logPane({ id: "l", lines: [], ariaLabel: "Log" }) }),
    progress({ title: "T", status: "S", state: "failed", percent: 50, startedAt: 0, endedAt: 1000 }),
    skeleton({ shape: "page", label: "L" }),
    emptyState({ line: "E", acts: [{ act: "a", label: "A" }] }),
  ].join("");
  assert.doesNotMatch(everything, /\sstyle=/);
});

test("the kit stylesheet is folded into every document's brand block, and names no colour of its own", () => {
  const css = kitStyles();
  assert.ok(brandStyleBlock().includes(css), "brandStyleBlock() carries kitStyles()");
  assert.doesNotMatch(css, /#[0-9a-fA-F]{3,8}\b/, "every colour is a token");
  assert.match(css, /\.mq-bar-fill\[data-percent="0"\] \{ width: 0%; \}/);
  assert.match(css, /\.mq-bar-fill\[data-percent="100"\] \{ width: 100%; \}/);
  assert.doesNotMatch(css, /body\.vscode-dark/, "the palette is chosen by the stamped attribute, never by the editor's class");
  for (const token of ["--memql-control-h", "--memql-radius", "--memql-motion-dur", "--memql-focus", "--memql-warn"]) {
    assert.ok(css.includes(`var(${token}`), `the kit reads ${token}`);
  }
});

test("pageDocument is one CSP'd document with the brand block and the one runtime", () => {
  const html = pageDocument({
    nonce: "n0nce",
    title: "Local <cluster>",
    themeAttr: ' data-memql-theme="dark"',
    screen: "landing",
    escapeAct: "back",
    head: head({ title: "Local cluster" }),
    body: "<p>body</p>",
    actions: actionBar({ state: "Ready", acts: [] }),
  });
  assert.match(html, /content="default-src 'none'; style-src 'nonce-n0nce'; script-src 'nonce-n0nce';"/);
  assert.match(html, /<meta name="memql-screen" content="landing">/);
  assert.match(html, /<title>Local &lt;cluster&gt;<\/title>/);
  assert.match(html, /<body data-memql-theme="dark" data-escape-act="back">/);
  assert.ok(html.includes(brandStyleBlock()), "the brand block, which carries the kit");
  assert.equal(html.split("<script").length - 1, 1, "exactly one script");
  assert.ok(html.includes(`<script nonce="n0nce">${PAGE_RUNTIME}</script>`), "the page runtime under the nonce");
  assert.doesNotMatch(html.replace(/<style[\s\S]*<\/style>/, ""), /\sstyle=/);
});
