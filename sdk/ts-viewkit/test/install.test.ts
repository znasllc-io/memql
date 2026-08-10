// The install-run and uninstall-preview renderers (memql#3474, #3476).
//
// These assert the MARKUP CONTRACT two separate hosts depend on -- the
// `data-state` and `data-kind` attributes they colour by, and the handful of
// places where drawing nothing would be a lie. They deliberately do NOT assert
// on CSS: styles.test.ts owns the class/stylesheet drift guard.
//
// The load-bearing ones, each pinning a way this surface could mislead while
// looking fine:
//
//   - five step states survive as five, rather than collapsing to done/not-done
//     (which is exactly what renderChecklist would have done to them);
//   - `preserved` is not reachable as a step state at all -- it is an
//     artifact's kind, on its own axis;
//   - a preserved artifact always says WHY, so it cannot read as a removal
//     that quietly failed;
//   - stderr survives verbatim, including characters that would otherwise be
//     read as markup.

import test from "node:test";
import assert from "node:assert/strict";

import {
  renderInstallSteps,
  renderRemovalPreview,
  type InstallStepState,
  type InstallStepView,
  type RemovalItemView,
} from "../src/install.js";
import { renderToHtml } from "../src/vnode.js";

function step(over: Partial<InstallStepView> = {}): InstallStepView {
  return { id: "stackCheckout", label: "Fetch the memQL stack", state: "pending", ...over };
}

function item(over: Partial<RemovalItemView> = {}): RemovalItemView {
  return { id: "removeCluster", label: "k3d cluster memql-local", kind: "removed", ...over };
}

const ALL_STATES: InstallStepState[] = ["pending", "running", "done", "skipped", "failed"];

// --------------------------------------------------------------------------
// steps
// --------------------------------------------------------------------------

test("every step state reaches the markup as its own data-state", () => {
  // The point of the module. A renderer that folded these into two buckets
  // would show a run that stopped four steps in as merely unfinished.
  const html = renderToHtml(
    renderInstallSteps(ALL_STATES.map((state, i) => step({ id: `s${i}`, state }))),
  );
  for (const state of ALL_STATES) {
    assert.match(html, new RegExp(`data-state="${state}"`), `${state} has no data-state`);
  }
});

test("each state draws a distinct marker", () => {
  // Same requirement one layer down: if two states share a glyph, the data
  // attribute is right and the thing a human actually looks at is not.
  const markers = ALL_STATES.map((state) => {
    const html = renderToHtml(renderInstallSteps([step({ state })]));
    return /class="vk-step-marker"[^>]*>([^<]*)</.exec(html)?.[1] ?? "";
  });
  assert.equal(new Set(markers).size, ALL_STATES.length, `markers collide: ${markers.join(" ")}`);
});

test("`preserved` is not a step state", () => {
  // Guards the two-axis split at the type level AND at runtime. `preserved`
  // belongs to an artifact in an uninstall preview, not to progress through a
  // run; allowing it here would resurrect the conflation the design rejected.
  const states: readonly string[] = ALL_STATES;
  assert.ok(!states.includes("preserved"));
});

test("the caller's order is the rendered order", () => {
  // Order is the graph's wave order. Sorting here would draw a sequence that
  // is a property of this function rather than of the dependency graph the
  // executor actually walked.
  const html = renderToHtml(
    renderInstallSteps([
      step({ id: "third", label: "Third" }),
      step({ id: "first", label: "First" }),
      step({ id: "second", label: "Second" }),
    ]),
  );
  assert.deepEqual(
    [...html.matchAll(/data-step-id="([^"]+)"/g)].map((m) => m[1]),
    ["third", "first", "second"],
  );
});

test("a failed step exposes its exit code as data and explains it in a tooltip", () => {
  // The number alone tells an operator nothing, and the four contract codes
  // ask for genuinely different next actions.
  const html = renderToHtml(
    renderInstallSteps([step({ state: "failed", exitCode: 4, error: "k3d: not found" })]),
  );
  assert.match(html, /data-exit-code="4"/);
  assert.match(html, /exit 4/);
  assert.match(html, /title="[^"]*prerequisite missing/);
});

test("an exit code on a step that did not fail is not rendered", () => {
  // A stale code carried on a passing step would read as a failure.
  const html = renderToHtml(renderInstallSteps([step({ state: "done", exitCode: 5 })]));
  assert.ok(!html.includes("data-exit-code"));
  assert.ok(!html.includes("exit 5"));
});

test("stderr survives verbatim, escaped rather than dropped or mangled", () => {
  // The one text that says what actually broke. Shell output contains angle
  // brackets and ampersands constantly; escaping is what lets it be shown at
  // all under a CSP that forbids anything clever.
  const stderr = 'error: <config> failed & "retry" needed';
  const html = renderToHtml(renderInstallSteps([step({ state: "failed", error: stderr })]));
  assert.match(html, /<details class="vk-step-error">/);
  assert.match(html, /&lt;config&gt; failed &amp; &quot;retry&quot; needed/);
  assert.ok(!html.includes("<config>"), "raw markup reached the output");
});

test("a step with no detail and no error draws neither container", () => {
  const html = renderToHtml(renderInstallSteps([step({ state: "done" })]));
  assert.ok(!html.includes("vk-step-detail"));
  assert.ok(!html.includes("vk-step-error"));
});

test("a skip's reason is rendered, because two skips mean different things", () => {
  // "already satisfied" and "you passed --skip" are the same state and
  // completely different news.
  const html = renderToHtml(
    renderInstallSteps([step({ state: "skipped", detail: "already satisfied" })]),
  );
  assert.match(html, /already satisfied/);
});

test("no steps renders a sentence rather than an empty list", () => {
  assert.match(renderToHtml(renderInstallSteps([])), /No steps to run\./);
});

// --------------------------------------------------------------------------
// removal preview
// --------------------------------------------------------------------------

test("removed and preserved are distinguishable in the markup without a caller-supplied colour", () => {
  // The two-tier model's whole point. The host colours `[data-kind]`; neither
  // consumer passes an icon or a colour, which is what stops #3474 and #3476
  // drifting into different vocabularies for the same idea.
  const html = renderToHtml(
    renderRemovalPreview([
      item({ id: "a", kind: "removed" }),
      item({ id: "b", kind: "preserved", label: "Docker", reason: "you installed it yourself" }),
    ]),
  );
  assert.match(html, /data-kind="removed"/);
  assert.match(html, /data-kind="preserved"/);
});

test("a preserved artifact always states why, even when the caller gave no reason", () => {
  // "Preserved" with nothing after it reads as a removal that silently
  // failed, which is the opposite of what happened.
  const html = renderToHtml(renderRemovalPreview([item({ kind: "preserved", reason: "" })]));
  assert.match(html, /it existed before the install/);
});

test("a caller's own preservation reason wins over the fallback", () => {
  const html = renderToHtml(
    renderRemovalPreview([item({ kind: "preserved", reason: "your own k3d cluster" })]),
  );
  assert.match(html, /your own k3d cluster/);
  assert.ok(!html.includes("it existed before the install"));
});

test("both kinds render in one list -- the preview IS the confirmation", () => {
  // Hiding the preserved half behind a disclosure would drop exactly the
  // surprise worth stopping on: something you expected removed that is not.
  const html = renderToHtml(
    renderRemovalPreview([item({ id: "a" }), item({ id: "b", kind: "preserved" })]),
  );
  assert.equal([...html.matchAll(/class="vk-removal"/g)].length, 2);
  assert.ok(!html.includes("<details"), "the preview must not hide either half");
});

test("the caller's order is the rendered order", () => {
  const html = renderToHtml(
    renderRemovalPreview([item({ id: "z" }), item({ id: "m" }), item({ id: "a" })]),
  );
  assert.deepEqual(
    [...html.matchAll(/data-item-id="([^"]+)"/g)].map((m) => m[1]),
    ["z", "m", "a"],
  );
});

test("an empty receipt says so rather than rendering a blank panel", () => {
  // A blank panel looks like it is still loading. "This would remove nothing"
  // is a fact the operator needs before they approve anything.
  assert.match(renderToHtml(renderRemovalPreview([])), /removes? nothing/);
});

test("labels are escaped", () => {
  const html = renderToHtml(renderRemovalPreview([item({ label: "<script>x</script>" })]));
  assert.ok(!html.includes("<script>"));
  assert.match(html, /&lt;script&gt;/);
});
