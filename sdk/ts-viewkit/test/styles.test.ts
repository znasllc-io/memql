// The markup contract and the stylesheet must not drift apart.
//
// view-kit emits class names AND owns what they mean. A class emitted by a
// renderer with no rule in the stylesheet is an unstyled element in every
// consumer, and the consumer's only recourse is to invent a rule -- at which
// point the same class means two different things in two codebases. These
// tests are the mechanism that stops that.
//
// The primary guard reads the renderer SOURCE rather than rendered output:
// output only covers the branches a fixture happens to reach, so a class added
// inside an uncovered branch would slip through. Scanning the source catches
// every `class: "vk-..."` literal regardless of reachability.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";

import { renderRowList } from "../src/rowList.js";
import { renderDetail } from "../src/detail.js";
import { renderToHtml } from "../src/vnode.js";
import { viewKitStyles, VIEW_KIT_CSS_VARIABLES } from "../src/styles.js";

// Compiled tests live at dist-test/test/, so the source tree is two levels up.
const SRC_DIR = path.resolve(fileURLToPath(import.meta.url), "../../../src");

// Every class named as a selector in the stylesheet. Comments are stripped
// first: a class NAMED in prose is not a class STYLED, and counting it would
// let a missing rule pass because the doc comment happened to mention it.
function styledClasses(css: string): Set<string> {
  const code = css.replace(/\/\*[\s\S]*?\*\//g, "");
  return new Set(code.match(/\.(vk-[a-z-]+)/g)?.map((m) => m.slice(1)) ?? []);
}

// Every class any renderer can put on an element, read off the source. Excludes
// styles.ts itself -- that is the thing being checked against, not a source of
// truth for what is emitted.
function emittableClasses(): Set<string> {
  const out = new Set<string>();
  for (const file of fs.readdirSync(SRC_DIR)) {
    if (!file.endsWith(".ts") || file === "styles.ts") continue;
    const src = fs.readFileSync(path.join(SRC_DIR, file), "utf8");
    // Matches `class: "a b"` and `"class": "a b"` (the attrs-record spellings)
    // and splits multi-class values.
    for (const m of src.matchAll(/"?class"?\s*:\s*"([^"]+)"/g)) {
      for (const cls of m[1].split(/\s+/)) {
        if (cls.startsWith("vk-")) out.add(cls);
      }
    }
  }
  return out;
}

test("the source tree really does emit view-kit classes (guards the scanner itself)", () => {
  const emitted = emittableClasses();
  assert.ok(
    emitted.size >= 12,
    `expected the renderers to emit at least 12 vk- classes, found ${emitted.size}: ` +
      `${[...emitted].join(", ")}. A near-empty set means the source scan silently ` +
      `stopped matching and every assertion below is vacuous.`,
  );
  // Spot-check one from each renderer so a regex that matched the wrong thing
  // cannot pass on volume alone.
  assert.ok(emitted.has("vk-row"), "expected rowList's vk-row");
  assert.ok(emitted.has("vk-key"), "expected detail's vk-key");
});

test("every class the renderers can emit has a rule in the stylesheet", () => {
  const styled = styledClasses(viewKitStyles);
  const missing = [...emittableClasses()].filter((c) => !styled.has(c)).sort();
  assert.deepEqual(
    missing,
    [],
    `these classes are emitted by a renderer but have no rule in styles.ts: ` +
      `${missing.join(", ")}. Add a rule -- an unstyled class forces every ` +
      `consumer to invent its own meaning for it.`,
  );
});

test("the stylesheet has no rule for a class no renderer emits", () => {
  const emitted = emittableClasses();
  const dead = [...styledClasses(viewKitStyles)].filter((c) => !emitted.has(c)).sort();
  assert.deepEqual(
    dead,
    [],
    `these classes have rules in styles.ts but are emitted by nothing: ` +
      `${dead.join(", ")}. Either a renderer stopped emitting it or the rule ` +
      `was written against a name that never existed.`,
  );
});

// The source scan above is the drift guard; this closes the loop from the
// other end, on real rendered output, so the two cannot both be wrong in the
// same direction.
test("every class in rendered output is styled", () => {
  const rowsHtml = renderToHtml(
    renderRowList(
      [{ id: "a1", name: "Sofia", role: "hr", active: "true" }],
      { id: "v1:agents:agent", entity: "agent",
        displayCard: { primary: "name", secondary: "role", tertiary: "id", status: "active" } },
      "a1",
    ),
  );
  const emptyHtml = renderToHtml(
    renderRowList([], { id: "v1:agents:agent", entity: "agent" }),
  );
  // Exercises every detail branch: scalar, null, nested object, array, empty
  // object, empty array, and a cycle.
  const cyclic: Record<string, unknown> = { id: "a1" };
  cyclic["self"] = cyclic;
  const detailHtml = renderToHtml(
    renderDetail({
      id: "a1",
      name: "Sofia",
      count: 3,
      active: true,
      missing: null,
      payload: { nested: "x" },
      tags: ["a"],
      emptyObj: {},
      emptyArr: [],
      loop: cyclic,
    }),
  );

  const styled = styledClasses(viewKitStyles);
  const rendered = new Set<string>();
  for (const html of [rowsHtml, emptyHtml, detailHtml]) {
    for (const m of html.matchAll(/class="([^"]+)"/g)) {
      for (const cls of m[1].split(/\s+/)) rendered.add(cls);
    }
  }

  assert.ok(rendered.size > 0, "the fixtures rendered no classes at all");
  const missing = [...rendered].filter((c) => !styled.has(c)).sort();
  assert.deepEqual(missing, [], `rendered but unstyled: ${missing.join(", ")}`);
});

test("the stylesheet is themed only through vk- custom properties", () => {
  // The whole point of the token layer: view-kit must not reach for a host's
  // variables. A `--vscode-*` here would couple the package to one consumer.
  const foreign = viewKitStyles.match(/var\(\s*(--(?!vk-)[a-z0-9-]+)/gi) ?? [];
  assert.deepEqual(
    foreign,
    [],
    `styles.ts references non-vk custom properties: ${foreign.join(", ")}. ` +
      `Hosts map their own variables onto --vk-* instead.`,
  );
});

test("every var() reference carries a fallback and is a documented token", () => {
  const documented = new Set<string>(VIEW_KIT_CSS_VARIABLES);
  for (const m of viewKitStyles.matchAll(/var\(\s*(--vk-[a-z0-9-]+)\s*([,)])/g)) {
    const [, name, delimiter] = m;
    assert.ok(
      documented.has(name),
      `${name} is used in styles.ts but missing from VIEW_KIT_CSS_VARIABLES, ` +
        `so a consumer theming view-kit would never know to set it.`,
    );
    // A fallback means a host that defines nothing still renders legibly.
    assert.equal(delimiter, ",", `var(${name}) is missing a fallback value`);
  }
});

test("every documented token is actually used", () => {
  for (const name of VIEW_KIT_CSS_VARIABLES) {
    assert.ok(
      viewKitStyles.includes(`var(${name},`),
      `${name} is advertised in VIEW_KIT_CSS_VARIABLES but setting it would do ` +
        `nothing -- styles.ts never reads it.`,
    );
  }
});

test("the stylesheet declares no page chrome", () => {
  // view-kit renders rows, not pages. A rule here for a toolbar, a pane, or a
  // bare element selector would have view-kit dictating layout it cannot see.
  // Strip comments BEFORE splitting: a comment sits between the previous
  // rule's `}` and the next selector, so it would otherwise be glued onto that
  // selector and read as part of it.
  const selectors = viewKitStyles
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .split("}")
    .map((block) => block.split("{")[0].trim())
    .filter((s) => s !== "");
  for (const selector of selectors) {
    for (const part of selector.split(",")) {
      assert.match(
        part.trim(),
        /^\.vk-/,
        `"${part.trim()}" is not a view-kit class selector. The consumer owns ` +
          `page chrome and bare element styling.`,
      );
    }
  }
});

test("the stylesheet is a plain string and injects nothing", () => {
  // view-kit is DOM-free: importing it must not have touched a document.
  assert.equal(typeof viewKitStyles, "string");
  assert.equal(typeof (globalThis as Record<string, unknown>)["document"], "undefined");
});
