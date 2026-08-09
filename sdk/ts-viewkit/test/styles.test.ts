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
import { SRC_DIR, sourceFiles, stripComments } from "./support/source.js";

import { renderRowList } from "../src/rowList.js";
import { renderDetail } from "../src/detail.js";
import { renderToHtml } from "../src/vnode.js";
import { renderElement } from "../src/fitness.js";
import { VIEW_KIT_ELEMENTS } from "../src/elements.js";
import { renderCalendar } from "../src/calendar.js";
import { renderPieChart } from "../src/chart.js";
import { viewKitStyles, VIEW_KIT_CSS_VARIABLES } from "../src/styles.js";

// Every class named as a selector in the stylesheet. Comments are stripped
// first: a class NAMED in prose is not a class STYLED, and counting it would
// let a missing rule pass because the doc comment happened to mention it.
function styledClasses(css: string): Set<string> {
  const code = css.replace(/\/\*[\s\S]*?\*\//g, "");
  // The character class includes digits: the chart palette's slots are
  // `.vk-chart-s1` .. `.vk-chart-s6`, and a letters-only pattern would read
  // them all as `vk-chart-s` and pass on a class that has no rule.
  return new Set(code.match(/\.(vk-[a-z0-9-]+)/g)?.map((m) => m.slice(1)) ?? []);
}

// Every class any renderer can put on an element, read off the source.
// Excludes styles.ts itself -- that is the thing being checked against, not a
// source of truth for what is emitted.
//
// Comments are stripped first, then every `vk-` token in what remains is
// collected. Scanning only `class: "literal"` missed the composed spellings
// (`\`vk-chart-line ${cls}\``), which is how a whole family of chart classes
// could have shipped unstyled. `data-vk-*` attributes are excluded by the
// lookbehind: they are host hooks, not classes.
function emittableClasses(): Set<string> {
  const out = new Set<string>();
  for (const { source } of sourceFiles(["styles.ts"])) {
    for (const m of stripComments(source).matchAll(/(?<!data-)\bvk-[a-z0-9-]+/g)) {
      out.add(m[0]);
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

  // Every element in the library, over a row set shaped to reach its slots.
  // The list is derived from VIEW_KIT_ELEMENTS rather than written out, so a
  // new element cannot be added without appearing here.
  const rows = [
    {
      id: "r1",
      name: "Alpha",
      status: "open",
      note: "first",
      done: false,
      at: "2026-08-04T09:00:00Z",
      endsAt: "2026-08-06T09:00:00Z",
      count: 12,
      latitude: 51.5,
      longitude: -0.12,
    },
    {
      id: "r2",
      name: "Beta",
      status: "closed",
      note: "second",
      done: true,
      at: "2026-08-05T11:30:00Z",
      count: 30,
      latitude: -33.86,
      longitude: 151.2,
    },
  ];
  const concept = {
    id: "x",
    entity: "widget",
    displayCard: { primary: "name", secondary: "note", tertiary: "at", status: "status" },
  };
  const elementHtml = VIEW_KIT_ELEMENTS.map((element) => {
    const node = renderElement(element, rows, concept, {
      selectedRowId: "r1",
      // Both coordinate slots are explicitOnly, so the map only renders when
      // a caller points at them -- which is what a composed view does.
      bindings: { latitude: "latitude", longitude: "longitude", end: "endsAt" },
    });
    assert.ok(node, `${element.id} did not fit the styling fixture, so it is unchecked`);
    return renderToHtml(node);
  });
  // The seventh-and-beyond fold has its own class, reachable only past the
  // palette's six slots.
  const foldHtml = renderToHtml(
    renderPieChart(
      Array.from({ length: 8 }, (_, i) => ({ id: `f${i}`, kind: `k${i}`, n: 8 - i })),
      { id: "x", entity: "widget" },
      { bindings: { category: "kind", value: "n" } },
    ),
  );
  // A blank calendar cell exists only in a month whose 1st is not a Sunday.
  const calendarHtml = renderToHtml(
    renderCalendar(rows, concept, { month: "2026-08", bindings: { start: "at" } }),
  );

  const styled = styledClasses(viewKitStyles);
  const rendered = new Set<string>();
  for (const html of [rowsHtml, emptyHtml, detailHtml, foldHtml, calendarHtml, ...elementHtml]) {
    for (const m of html.matchAll(/class="([^"]+)"/g)) {
      for (const cls of m[1].split(/\s+/)) rendered.add(cls);
    }
  }

  assert.ok(rendered.size > 20, `the fixtures rendered only ${rendered.size} classes`);
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
    if (name.endsWith("-default")) {
      // The internal tier: view-kit's own per-theme answer, DEFINED in this
      // sheet rather than supplied by a host. It is the fallback, so it has
      // none of its own, and it must not be advertised as a host token --
      // setting it would be silently overridden by the host tier above it.
      assert.ok(
        !documented.has(name),
        `${name} is internal and must not appear in VIEW_KIT_CSS_VARIABLES.`,
      );
      assert.match(
        viewKitStyles,
        new RegExp(`${name}\\s*:`),
        `${name} is read but never defined, so the fallback resolves to nothing.`,
      );
      continue;
    }
    assert.ok(
      documented.has(name),
      `${name} is used in styles.ts but missing from VIEW_KIT_CSS_VARIABLES, ` +
        `so a consumer theming view-kit would never know to set it.`,
    );
    // A fallback means a host that defines nothing still renders legibly.
    assert.equal(delimiter, ",", `var(${name}) is missing a fallback value`);
  }
});

test("every themed default is defined for BOTH light and dark", () => {
  // The whole reason for the two-tier token: a chart hue cannot have one
  // theme-neutral fallback. A default defined only in the light block would
  // paint a light-mode hue on a dark surface and fail contrast.
  const light = viewKitStyles.slice(0, viewKitStyles.indexOf("@media"));
  const dark = viewKitStyles.slice(viewKitStyles.indexOf("@media"));
  const defaults = new Set(
    [...viewKitStyles.matchAll(/(--vk-[a-z0-9-]+-default)\s*:/g)].map((m) => m[1]),
  );
  assert.ok(defaults.size >= 10, `only ${defaults.size} themed defaults found`);
  for (const name of defaults) {
    assert.ok(light.includes(`${name}:`), `${name} has no light-mode value`);
    assert.ok(
      dark.includes(`${name}:`),
      `${name} is never redefined for dark mode, so the light value paints on a ` +
        `dark surface.`,
    );
    // Both dark scopes: the OS preference AND the explicit stamp. A host that
    // renders dark chrome on a light OS depends on the second one.
    assert.equal(
      (dark.match(new RegExp(`${name}\\s*:`, "g")) ?? []).length,
      2,
      `${name} must be redefined in both dark scopes (prefers-color-scheme and ` +
        `[data-vk-theme="dark"]).`,
    );
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
    // Unwrap at-rule blocks: `@media (...) { .vk-x { ... } }` would otherwise
    // glue the at-rule onto the first selector inside it. The rules INSIDE
    // still get checked -- only the wrapper is removed.
    .replace(/@media[^{]*\{/g, "")
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
