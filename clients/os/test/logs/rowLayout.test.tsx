import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { act } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import { ROW_HEIGHT, STACKED_ROW_HEIGHT, stackBelow, stackedRowHeight } from "../../src/logs/LogLine";
import { fakeConnection, logRows, renderAppLogs, renderLogsApp } from "./harness";

// The log row's two arrangements (R40, epic memql#5478). In a list narrower
// than its measure one line cannot hold the fixed cells and a readable message,
// so the row stacks on two -- and the windowed list must be TOLD, because it
// places every row by a fixed height: a row restyled to two lines that the
// list still spaced as one would overlap its neighbour. The measure follows
// the reader's font size, because the cells are sized in characters of
// rem-sized fonts. jsdom lays nothing out, so the list's width is stubbed and
// the root font size is set inline; the arrangement itself is judged as
// pixels (the rendered acceptance).

let listWidth = 0;
let observed: ResizeObserverCallback[] = [];

beforeEach(() => {
  h.connection = null;
  listWidth = 0;
  observed = [];
  const original = HTMLElement.prototype.getBoundingClientRect;
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
    if (!this.classList.contains("os-logs-list")) return original.call(this);
    return { x: 0, y: 0, top: 0, left: 0, bottom: 0, right: listWidth, width: listWidth, height: 0, toJSON: () => ({}) } as DOMRect;
  });
  // A ResizeObserver the test drives: a resize is the one moment the list is
  // measured again after it mounts.
  vi.stubGlobal(
    "ResizeObserver",
    class {
      constructor(callback: ResizeObserverCallback) {
        observed.push(callback);
      }
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
    },
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  document.documentElement.style.fontSize = "";
});

function list(): HTMLElement {
  const el = document.querySelector<HTMLElement>(".os-logs-list");
  if (el === null) throw new Error("no log list rendered");
  return el;
}

function rowHeights(): string[] {
  return [...document.querySelectorAll<HTMLElement>(".os-vlist-row")].map((row) => row.style.height);
}

/** The list resizes to `width`: every observer hears of it. */
function resizeTo(width: number): void {
  listWidth = width;
  act(() => {
    for (const callback of observed) callback([], {} as ResizeObserver);
  });
}

const AT_16 = stackBelow("comfortable", 16);

describe("an app's Logs section", () => {
  it.each([
    ["narrower than the line needs", AT_16 - 1, true, STACKED_ROW_HEIGHT.comfortable],
    ["exactly as wide as the line needs", AT_16, false, ROW_HEIGHT.comfortable],
    ["wide", 1200, false, ROW_HEIGHT.comfortable],
    // Unmeasured (a DOM with no layout) keeps the one line every row had.
    ["unmeasured", 0, false, ROW_HEIGHT.comfortable],
  ])("lays its rows out for a list %s", async (_what, width, stacked, height) => {
    listWidth = width;
    h.connection = fakeConnection({ tail: logRows(3, { attributes: { stepKey: "tests.unit" } }) });
    await renderAppLogs({ app: "deployables" });

    expect(list().hasAttribute("data-stacked")).toBe(stacked);
    const heights = rowHeights();
    expect(heights).toHaveLength(3);
    expect(heights.every((h) => h === `${height}px`)).toBe(true);
  });

  it("changes arrangement when the list is resized across its measure", async () => {
    listWidth = 1200;
    h.connection = fakeConnection({ tail: logRows(3) });
    await renderAppLogs({ app: "deployables" });
    expect(list().hasAttribute("data-stacked")).toBe(false);

    resizeTo(AT_16 - 1);
    expect(list().hasAttribute("data-stacked")).toBe(true);
    expect(rowHeights().every((h) => h === `${STACKED_ROW_HEIGHT.comfortable}px`)).toBe(true);

    resizeTo(AT_16);
    expect(list().hasAttribute("data-stacked")).toBe(false);
    expect(rowHeights().every((h) => h === `${ROW_HEIGHT.comfortable}px`)).toBe(true);
  });
});

describe("the measure follows the reader's font size", () => {
  it.each([
    // Chrome's "Large": the line needs more room, so a list that holds it at
    // the default font stacks.
    [20, AT_16, true],
    [20, stackBelow("comfortable", 20) - 1, true],
    [20, stackBelow("comfortable", 20), false],
    // "Small": less room is enough.
    [12, stackBelow("comfortable", 12) - 1, true],
    [12, stackBelow("comfortable", 12), false],
  ])("with a %ipx root and a %ipx list, stacked is %s", async (rootPx, width, stacked) => {
    document.documentElement.style.fontSize = `${rootPx}px`;
    listWidth = width;
    h.connection = fakeConnection({ tail: logRows(2) });
    await renderAppLogs({ app: "deployables" });
    expect(list().hasAttribute("data-stacked")).toBe(stacked);
    // Two stacked lines of text are a taller row under a larger font; one
    // line keeps its height.
    const height = stacked ? stackedRowHeight("comfortable", rootPx) : ROW_HEIGHT.comfortable;
    expect(rowHeights().every((h) => h === `${height}px`)).toBe(true);
  });

  it("sizes a stacked row for its font, from the default's proportions", () => {
    expect(stackedRowHeight("comfortable", 16)).toBe(STACKED_ROW_HEIGHT.comfortable);
    expect(stackedRowHeight("compact", 16)).toBe(STACKED_ROW_HEIGHT.compact);
    expect(stackedRowHeight("comfortable", 20)).toBe(60);
    expect(stackedRowHeight("compact", 24)).toBe(60);
  });

  it("scales only what is sized in characters", () => {
    // The gaps, padding, borders and scrollbar are pixels and stay pixels. A
    // measure scaled whole would ask too much under a large font and, under a
    // small one, too little: a line that overflows its row.
    expect(stackBelow("comfortable", 20)).toBeGreaterThan(AT_16);
    expect(stackBelow("comfortable", 20)).toBeLessThan(AT_16 * 1.25);
    expect(stackBelow("comfortable", 12)).toBeGreaterThan(AT_16 * 0.75);
  });
});

describe("the Logs app's Search", () => {
  it("decides by its own density's measure", async () => {
    // Between the two densities' widths: a compact list still fits one line
    // where a comfortable one would not.
    listWidth = Math.floor((stackBelow("compact", 16) + AT_16) / 2);
    h.connection = fakeConnection({ search: logRows(2) });
    const compact = await renderLogsApp({ section: "search", settings: { density: "compact" } });
    expect(list().hasAttribute("data-stacked")).toBe(false);
    expect(rowHeights().every((h) => h === `${ROW_HEIGHT.compact}px`)).toBe(true);
    compact.view.unmount();

    await renderLogsApp({ section: "search", settings: { density: "comfortable" } });
    expect(list().hasAttribute("data-stacked")).toBe(true);
    expect(rowHeights().every((h) => h === `${STACKED_ROW_HEIGHT.comfortable}px`)).toBe(true);
  });
});

describe("the stylesheet", () => {
  const here = dirname(fileURLToPath(import.meta.url));
  const css = readFileSync(join(here, "../../src/styles/index.css"), "utf8");
  function block(selector: string): string {
    const at = css.indexOf(`\n${selector} {`);
    if (at === -1) throw new Error(`no rule in index.css for selector: ${selector}`);
    const open = css.indexOf("{", at);
    return css.slice(open + 1, css.indexOf("}", open));
  }

  // The decision is the code's and the arrangement is the stylesheet's; these
  // are the strings they share.
  it("arranges the stacked row on the attribute the list carries", () => {
    expect(css).toContain(".os-logs-list[data-stacked] .os-logs-line {");
    expect(css).toContain(".os-logs-list[data-stacked] .os-logs-attrs { display: none; }");
  });

  it("holds the two widths STACK_BELOW was measured against", () => {
    // The message's minimum and the subject mark's bound: a change to either
    // is a change to the measure, which has to be taken again.
    expect(block(".os-logs-message")).toContain("min-width: 32ch;");
    expect(block(".os-logs-subject")).toContain("max-width: calc(18ch + 16px);");
    expect(block(".os-logs-subject")).toContain("text-overflow: ellipsis;");
  });

  it("gives the attributes a fixed column that shrinks first", () => {
    // The message grows into the row from nothing and stops at its minimum;
    // the attributes' basis is their whole column, so a wide row starts it at
    // one x, and they are what a short row takes its width from.
    expect(block(".os-logs-message")).toContain("flex: 1 1 0;");
    expect(block(".os-logs-attrs")).toContain("flex: 0 1 40%;");
    expect(block(".os-logs-attrs")).toContain("min-width: 0;");
    expect(block(".os-logs-attrs")).not.toContain("margin-left: auto");
  });
});
