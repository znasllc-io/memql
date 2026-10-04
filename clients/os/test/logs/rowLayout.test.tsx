import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { act, render } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import {
  ROW_HEIGHT,
  STACKED_ROW_HEIGHT,
  logLayout,
  stackBelow,
  stackedRowHeight,
  wideFrom,
} from "../../src/logs/LogLine";
import { useLogRowLayout } from "../../src/logs/useLogRowLayout";
import { fakeConnection, logRows, renderAppLogs, renderLogsApp } from "./harness";

// A log list's three layouts (R40, R40b, epic memql#5478), one per list and
// named on it as `data-layout`: WIDE, with the attributes an aligned column;
// MEDIUM, the message first; NARROW, each line on two rows. The windowed list
// must be TOLD the NARROW row height, because it places every row by a fixed
// height: a row restyled to two lines that the list still spaced as one would
// overlap its neighbour. Both measures follow the reader's font size, because
// the cells are sized in characters of rem-sized fonts. jsdom lays nothing
// out, so the list's width is stubbed and the root font size is set inline;
// the arrangements themselves are judged as pixels (the rendered acceptance).

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

const STACK_16 = stackBelow("comfortable", 16);
const WIDE_16 = wideFrom("comfortable", 16);
const STACK_20 = stackBelow("comfortable", 20);
const WIDE_20 = wideFrom("comfortable", 20);
const ONE_LINE = ROW_HEIGHT.comfortable;

describe("the layouts' boundaries", () => {
  it("are the measured widths, at 16px and 20px roots", () => {
    // The widths the captures were taken at. A change to either measure is
    // a change to these, and the captures have to be taken again.
    expect([STACK_16, WIDE_16]).toEqual([677, 1199]);
    expect([STACK_20, WIDE_20]).toEqual([820, 1461]);
    expect([stackBelow("compact", 16), wideFrom("compact", 16)]).toEqual([629, 1111]);
    expect([stackBelow("compact", 20), wideFrom("compact", 20)]).toEqual([763, 1356]);
  });

  it.each([16, 20])("divide the widths into the three layouts at a %ipx root", (rootPx) => {
    const stack = stackBelow("comfortable", rootPx);
    const wide = wideFrom("comfortable", rootPx);
    expect(logLayout(stack - 1, "comfortable", rootPx)).toBe("narrow");
    expect(logLayout(stack, "comfortable", rootPx)).toBe("medium");
    expect(logLayout(wide - 1, "comfortable", rootPx)).toBe("medium");
    expect(logLayout(wide, "comfortable", rootPx)).toBe("wide");
    // Unmeasured is the one line every row had before R40.
    expect(logLayout(0, "comfortable", rootPx)).toBe("wide");
  });

  it("scale only what is sized in characters", () => {
    // The gaps, padding, borders and scrollbar are pixels and stay pixels. A
    // measure scaled whole would ask too much under a large font and, under a
    // small one, too little: a line that overflows its row.
    const measures: [number, number, number][] = [
      [STACK_16, STACK_20, stackBelow("comfortable", 12)],
      [WIDE_16, WIDE_20, wideFrom("comfortable", 12)],
    ];
    for (const [at16, at20, at12] of measures) {
      expect(at20).toBeGreaterThan(at16);
      expect(at20).toBeLessThan(at16 * 1.25);
      expect(at12).toBeGreaterThan(at16 * 0.75);
    }
  });

  it("size a stacked row for its font, from the default's proportions", () => {
    expect(stackedRowHeight("comfortable", 16)).toBe(STACKED_ROW_HEIGHT.comfortable);
    expect(stackedRowHeight("compact", 16)).toBe(STACKED_ROW_HEIGHT.compact);
    expect(stackedRowHeight("comfortable", 20)).toBe(60);
    expect(stackedRowHeight("compact", 24)).toBe(60);
  });
});

describe("an app's Logs section", () => {
  it.each([
    [16, STACK_16 - 1, "narrow", STACKED_ROW_HEIGHT.comfortable],
    [16, STACK_16, "medium", ONE_LINE],
    [16, WIDE_16 - 1, "medium", ONE_LINE],
    [16, WIDE_16, "wide", ONE_LINE],
    [16, 0, "wide", ONE_LINE],
    [20, STACK_20 - 1, "narrow", stackedRowHeight("comfortable", 20)],
    [20, STACK_20, "medium", ONE_LINE],
    [20, WIDE_20 - 1, "medium", ONE_LINE],
    [20, WIDE_20, "wide", ONE_LINE],
    // A larger font moves both boundaries out: a list that is WIDE at the
    // default is MEDIUM under Chrome's "Large", and one that is MEDIUM stacks.
    [20, WIDE_16, "medium", ONE_LINE],
    [20, STACK_16, "narrow", stackedRowHeight("comfortable", 20)],
  ])("at a %ipx root, lays a %ipx list out %s", async (rootPx, width, layout, height) => {
    document.documentElement.style.fontSize = `${rootPx}px`;
    listWidth = width;
    h.connection = fakeConnection({ tail: logRows(3, { attributes: { stepKey: "tests.unit" } }) });
    await renderAppLogs({ app: "deployables" });

    expect(list().getAttribute("data-layout")).toBe(layout);
    const heights = rowHeights();
    expect(heights).toHaveLength(3);
    expect(heights.every((h) => h === `${height}px`)).toBe(true);
  });

  it("changes layout when the list is resized across a measure", async () => {
    listWidth = 1400;
    h.connection = fakeConnection({ tail: logRows(3) });
    await renderAppLogs({ app: "deployables" });
    expect(list().getAttribute("data-layout")).toBe("wide");

    resizeTo(WIDE_16 - 1);
    expect(list().getAttribute("data-layout")).toBe("medium");
    expect(rowHeights().every((h) => h === `${ONE_LINE}px`)).toBe(true);

    resizeTo(STACK_16 - 1);
    expect(list().getAttribute("data-layout")).toBe("narrow");
    expect(rowHeights().every((h) => h === `${STACKED_ROW_HEIGHT.comfortable}px`)).toBe(true);

    resizeTo(1400);
    expect(list().getAttribute("data-layout")).toBe("wide");
    expect(rowHeights().every((h) => h === `${ONE_LINE}px`)).toBe(true);
  });
});

describe("the hook", () => {
  it("renders again only when the layout or the row height changes", () => {
    let renders = 0;
    function Probe() {
      renders += 1;
      const arrangement = useLogRowLayout("comfortable");
      return <div className="os-logs-list" ref={arrangement.listRef} data-layout={arrangement.layout} />;
    }
    listWidth = 1400;
    render(<Probe />);
    const settled = renders;

    // Resizes inside one layout render nothing.
    resizeTo(1300);
    resizeTo(WIDE_16);
    expect(renders).toBe(settled);

    // Across a measure: one render, and none for the next resize inside it.
    resizeTo(WIDE_16 - 1);
    expect(renders).toBe(settled + 1);
    resizeTo(900);
    expect(renders).toBe(settled + 1);

    resizeTo(600);
    expect(list().getAttribute("data-layout")).toBe("narrow");
    expect(renders).toBe(settled + 2);

    // Still NARROW, but under a larger font the stacked row is taller: the
    // height changed, so the list renders once more.
    document.documentElement.style.fontSize = "20px";
    resizeTo(601);
    expect(renders).toBe(settled + 3);
    resizeTo(602);
    expect(renders).toBe(settled + 3);
  });
});

describe("the Logs app's Search", () => {
  it("decides by its own density's measures", async () => {
    // Between the two densities' stack widths: a compact list still fits one
    // line where a comfortable one stacks.
    listWidth = Math.floor((stackBelow("compact", 16) + STACK_16) / 2);
    h.connection = fakeConnection({ search: logRows(2) });
    const compact = await renderLogsApp({ section: "search", settings: { density: "compact" } });
    expect(list().getAttribute("data-layout")).toBe("medium");
    expect(rowHeights().every((h) => h === `${ROW_HEIGHT.compact}px`)).toBe(true);
    compact.view.unmount();

    await renderLogsApp({ section: "search", settings: { density: "comfortable" } });
    expect(list().getAttribute("data-layout")).toBe("narrow");
    expect(rowHeights().every((h) => h === `${STACKED_ROW_HEIGHT.comfortable}px`)).toBe(true);
  });

  it("is WIDE sooner in compact density, whose cells are smaller", async () => {
    listWidth = Math.floor((wideFrom("compact", 16) + WIDE_16) / 2);
    h.connection = fakeConnection({ search: logRows(2) });
    const compact = await renderLogsApp({ section: "search", settings: { density: "compact" } });
    expect(list().getAttribute("data-layout")).toBe("wide");
    compact.view.unmount();

    await renderLogsApp({ section: "search", settings: { density: "comfortable" } });
    expect(list().getAttribute("data-layout")).toBe("medium");
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

  // The decision is the code's and the arrangements are the stylesheet's;
  // these are the strings they share.
  it("arranges NARROW on the attribute the list carries", () => {
    expect(css).toContain('.os-logs-list[data-layout="narrow"] .os-logs-line {');
    expect(css).toContain('.os-logs-list[data-layout="narrow"] .os-logs-attrs { display: none; }');
  });

  it("holds the widths the measures were taken against", () => {
    // The message's minimum and the subject mark's bound: a change to either
    // is a change to both measures, which have to be taken again.
    expect(block(".os-logs-message")).toContain("min-width: 32ch;");
    expect(block(".os-logs-subject")).toContain("max-width: calc(18ch + 16px);");
    expect(block(".os-logs-subject")).toContain("text-overflow: ellipsis;");
  });

  it("gives WIDE an aligned attribute column", () => {
    // The base rules are WIDE's: the message grows into the row from nothing
    // and the attributes' basis is their whole column, which starts it at one
    // x on every line ending in the same mark.
    expect(block(".os-logs-message")).toContain("flex: 1 1 0;");
    expect(block(".os-logs-attrs")).toContain("flex: 0 1 40%;");
    expect(block(".os-logs-attrs")).toContain("min-width: 0;");
    expect(block(".os-logs-attrs")).not.toContain("margin-left: auto");
  });

  it("puts the message first in MEDIUM", () => {
    // The message sized by its own length; the attributes with no basis,
    // taking only what it leaves.
    expect(block('.os-logs-list[data-layout="medium"] .os-logs-message')).toContain("flex: 0 1 auto;");
    const attrs = block('.os-logs-list[data-layout="medium"] .os-logs-attrs');
    expect(attrs).toContain("flex: 1 1 0;");
    expect(attrs).toContain("margin-left: auto;");
  });
});
