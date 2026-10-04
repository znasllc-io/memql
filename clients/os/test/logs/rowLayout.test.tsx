import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import { ROW_HEIGHT, STACKED_ROW_HEIGHT, STACK_BELOW } from "../../src/logs/LogLine";
import { fakeConnection, logRows, renderAppLogs, renderLogsApp } from "./harness";

// The log row's two arrangements (R40, epic memql#5478). In a list narrower
// than STACK_BELOW one line cannot hold the fixed cells and a readable message,
// so the row stacks on two -- and the windowed list must be TOLD, because it
// places every row by a fixed height: a row restyled to two lines that the
// list still spaced as one would overlap its neighbour. jsdom lays nothing
// out, so the list's measured width is stubbed; the arrangement itself is
// judged as pixels (the rendered acceptance).

let listWidth = 0;

beforeEach(() => {
  h.connection = null;
  listWidth = 0;
  const original = HTMLElement.prototype.getBoundingClientRect;
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
    if (!this.classList.contains("os-logs-list")) return original.call(this);
    return { x: 0, y: 0, top: 0, left: 0, bottom: 0, right: listWidth, width: listWidth, height: 0, toJSON: () => ({}) } as DOMRect;
  });
});

afterEach(() => {
  vi.restoreAllMocks();
});

function list(): HTMLElement {
  const el = document.querySelector<HTMLElement>(".os-logs-list");
  if (el === null) throw new Error("no log list rendered");
  return el;
}

function rowHeights(): string[] {
  return [...document.querySelectorAll<HTMLElement>(".os-vlist-row")].map((row) => row.style.height);
}

describe("an app's Logs section", () => {
  it.each([
    ["narrower than the line needs", STACK_BELOW.comfortable - 1, true, STACKED_ROW_HEIGHT.comfortable],
    ["exactly as wide as the line needs", STACK_BELOW.comfortable, false, ROW_HEIGHT.comfortable],
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
});

describe("the Logs app's Search", () => {
  it("decides by its own density's measure", async () => {
    // Between the two densities' widths: a compact list still fits one line
    // where a comfortable one would not.
    listWidth = Math.floor((STACK_BELOW.compact + STACK_BELOW.comfortable) / 2);
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
  // The decision is the code's and the arrangement is the stylesheet's; this
  // is the one string they share.
  it("arranges the stacked row on the attribute the list carries", () => {
    const here = dirname(fileURLToPath(import.meta.url));
    const css = readFileSync(join(here, "../../src/styles/index.css"), "utf8");
    expect(css).toContain(".os-logs-list[data-stacked] .os-logs-line {");
    expect(css).toContain(".os-logs-list[data-stacked] .os-logs-attrs { display: none; }");
  });
});
