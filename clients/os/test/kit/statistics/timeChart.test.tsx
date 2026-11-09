import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { TimeChart, type ChartPoint } from "../../../src/kit/statistics/TimeChart";

afterEach(cleanup);

// THE ONE TIME-SERIES CHART (spec 8). Every statistics page draws its series
// through it, so what it promises is promised everywhere: a bucket nothing
// measured is a GAP in the line, never a zero; the previous period is a faint
// dashed line under the current one; hover, focus and the arrow keys move one
// readout; and a screen reader gets the whole series in one sentence.

const count = (value: number) => value.toLocaleString("en-US");
const CSS = readFileSync(join(__dirname, "..", "..", "..", "src", "kit", "statistics", "statistics.css"), "utf8");

/** Daily points from `start`, one per value. */
function days(values: readonly (number | null)[], start = "2026-11-02"): ChartPoint[] {
  const base = Date.parse(`${start}T00:00:00Z`);
  return values.map((value, index) => ({ at: new Date(base + index * 86_400_000).toISOString(), value }));
}

function seriesPath(container: HTMLElement, series: "current" | "previous"): SVGPathElement | null {
  return container.querySelector(`path[data-series="${series}"]`);
}

function commands(d: string, command: string): number {
  return d.split("").filter((char) => char === command).length;
}

describe("the line", () => {
  it("null points are gaps, not zeroes", () => {
    const { container } = render(<TimeChart label="Visitors" bucket="day" points={days([3, 5, null, 4, 6])} format={count} />);
    const d = seriesPath(container, "current")?.getAttribute("d") ?? "";
    // Two runs, each starting with its own M. A null drawn as a zero would be
    // one run of five points dipping to the baseline.
    expect(commands(d, "M")).toBe(2);
    expect(commands(d, "M") + commands(d, "L")).toBe(4);
    // The soft fill under the line breaks at the same place.
    const area = container.querySelector('path[data-area="current"]')?.getAttribute("d") ?? "";
    expect(commands(area, "M")).toBe(2);
  });

  it("an all null series draws no line", () => {
    const { container } = render(<TimeChart label="Visitors" bucket="day" points={days([null, null, null])} format={count} />);
    expect(seriesPath(container, "current")).toBeNull();
    expect(container.querySelector('path[data-area="current"]')).toBeNull();
    // No scale is offered for nothing: three labels reading 0, 0.5 and 1
    // would be a claim about values nobody measured.
    expect(container.querySelector(".os-time-chart-values")?.textContent).toBe("");
    // With no scale there are no steps to rule: the baseline alone.
    expect(container.querySelectorAll(".os-time-chart-grid")).toHaveLength(1);
    expect(container.querySelector(".os-time-chart-empty")?.textContent).toBe("Not measured");
  });

  it("a point with a gap either side is still drawn", () => {
    // The first day visitors were counted, say: one measured bucket at the end.
    const { container } = render(<TimeChart label="Visitors" bucket="day" points={days([null, null, 7])} format={count} />);
    expect(container.querySelectorAll(".os-time-chart-point")).toHaveLength(1);
  });

  it("stretches to its width and keeps its height", () => {
    const { container, rerender } = render(<TimeChart label="Visitors" bucket="day" points={days([1, 2])} format={count} />);
    const svg = container.querySelector("figure svg");
    expect(svg?.getAttribute("viewBox")).toBe("0 0 600 160");
    expect(svg?.getAttribute("preserveAspectRatio")).toBe("none");
    rerender(<TimeChart label="Visitors" bucket="day" points={days([1, 2])} format={count} height={120} />);
    expect(container.querySelector("figure svg")?.getAttribute("viewBox")).toBe("0 0 600 120");
  });
});

describe("the previous period", () => {
  it("the previous period is dashed", () => {
    const { container, rerender } = render(
      <TimeChart label="Visitors" bucket="day" points={days([3, 5, 4])} previous={days([2, 4, 6], "2026-10-30")} format={count} />,
    );
    const previous = seriesPath(container, "previous");
    expect(previous).not.toBeNull();
    expect(previous?.getAttribute("stroke-dasharray")).toMatch(/\d/);
    expect(seriesPath(container, "current")?.getAttribute("stroke-dasharray")).toBeNull();
    // A dashed line needs saying once what it is.
    expect(container.querySelector(".os-time-chart-legend")?.textContent).toBe("VisitorsPrevious period");

    // Twelve months has no previous period: no line, and nothing to explain.
    rerender(<TimeChart label="Visitors" bucket="month" points={days([3, 5, 4])} format={count} />);
    expect(seriesPath(container, "previous")).toBeNull();
    expect(container.querySelector(".os-time-chart-legend")).toBeNull();

    // Nor when the previous period measured nothing: there is no line to explain.
    rerender(<TimeChart label="Visitors" bucket="day" points={days([3, 5, 4])} previous={days([null, null, null], "2026-10-30")} format={count} />);
    expect(seriesPath(container, "previous")).toBeNull();
    expect(container.querySelector(".os-time-chart-legend")).toBeNull();
  });

  it("shares the value axis, so a larger previous period still fits", () => {
    const { container } = render(
      <TimeChart label="Visitors" bucket="day" points={days([3, 5, 4])} previous={days([2, 40, 6], "2026-10-30")} format={count} />,
    );
    expect([...container.querySelectorAll(".os-time-chart-values > span")].map((span) => span.textContent)).toEqual(["50", "25", "0"]);
  });

  it("draws a previous bucket with a gap either side as a dot, and says what it is", () => {
    // A previous period made only of lone buckets has no stroke to draw.
    const { container } = render(
      <TimeChart label="Visitors" bucket="day" points={days([3, 5, 4])} previous={days([null, 4, null], "2026-10-30")} format={count} />,
    );
    expect(seriesPath(container, "previous")).toBeNull();
    expect(container.querySelectorAll('.os-time-chart-point[data-series="previous"]')).toHaveLength(1);
    expect(container.querySelector(".os-time-chart-legend")?.textContent).toBe("VisitorsPrevious period");
  });
});

describe("the axes", () => {
  it("never labels half of a count", () => {
    // A small count of visitors: zero and the top, never "2.5".
    const { container, rerender } = render(<TimeChart label="Visitors" bucket="day" points={days([1, 3, 2])} format={count} />);
    expect([...container.querySelectorAll(".os-time-chart-values > span")].map((span) => span.textContent)).toEqual(["5", "0"]);
    expect(container.querySelectorAll(".os-time-chart-grid")).toHaveLength(2);
    // A rate keeps its middle step.
    const percent = (value: number) => `${Math.round(value * 1000) / 10}%`;
    rerender(<TimeChart label="Click rate" bucket="day" points={days([0.01, 0.03, 0.02])} format={percent} />);
    expect([...container.querySelectorAll(".os-time-chart-values > span")].map((span) => span.textContent)).toEqual(["5%", "2.5%", "0%"]);
  });

  it("rules no line across the words when this period was not measured and the previous one was", () => {
    const { container } = render(
      <TimeChart label="Visitors" bucket="day" points={days([null, null, null])} previous={days([20, 40, 30], "2026-10-30")} format={count} />,
    );
    expect(container.querySelector(".os-time-chart-empty")?.textContent).toBe("Not measured");
    // The top and the baseline only: the middle rule would run through the words.
    const rules = [...container.querySelectorAll(".os-time-chart-grid")].map((line) => Number(line.getAttribute("y1")));
    expect(rules.sort((a, b) => a - b)).toEqual([0, 160]);
    expect([...container.querySelectorAll(".os-time-chart-values > span")].map((span) => span.textContent)).toEqual(["50", "0"]);
  });

  it("labels the value axis in three steps, through the page's format", () => {
    const { container, rerender } = render(<TimeChart label="Visitors" bucket="day" points={days([120, 230, 90])} format={count} />);
    expect([...container.querySelectorAll(".os-time-chart-values > span")].map((span) => span.textContent)).toEqual(["250", "125", "0"]);
    const percent = (value: number) => `${Math.round(value * 100)}%`;
    rerender(<TimeChart label="Bounce rate" bucket="day" points={days([0.31, 0.42, 0.38])} format={percent} />);
    expect([...container.querySelectorAll(".os-time-chart-values > span")].map((span) => span.textContent)).toEqual(["50%", "25%", "0%"]);
  });

  it("labels the first and the last bucket of the time axis, and the ones between that fit", () => {
    const { container } = render(<TimeChart label="Visitors" bucket="day" points={days([1, 2, 3, 4, 5, 6, 7])} format={count} />);
    const labels = [...container.querySelectorAll(".os-time-chart-times > span")].map((span) => span.textContent);
    expect(labels[0]).toBe("Nov 2");
    expect(labels.at(-1)).toBe("Nov 8");
    expect(labels).toHaveLength(7);
  });

  it("labels hours by the hour, and names their day wherever there is no axis beside them", () => {
    // Twenty-four hours from 15:00 yesterday to 15:00 today.
    const hours = Array.from({ length: 25 }, (_, index) => ({ at: new Date(Date.parse("2026-11-07T15:00:00Z") + index * 3_600_000).toISOString(), value: index === 12 ? 70 : 10 }));
    const { container } = render(<TimeChart label="Page views" bucket="hour" points={hours} format={count} />);
    const axis = [...container.querySelectorAll(".os-time-chart-times > span")].map((span) => span.textContent);
    expect(axis[0]).toBe("15:00");
    expect(axis.at(-1)).toBe("15:00");
    expect(container.querySelector("figcaption")?.textContent).toBe("Page views: 25 points from Nov 7, 15:00 to Nov 8, 15:00; highest 70 on Nov 8, 03:00.");
    const plot = screen.getByRole("application", { name: "Page views" });
    act(() => plot.focus());
    expect(screen.getByRole("status").textContent).toBe("Nov 8, 15:00: 10");
  });
});

describe("the readout", () => {
  it("arrow keys move the readout", () => {
    render(
      <TimeChart label="Visitors" bucket="day" points={days([3, 5, null, 4, 6])} previous={days([2, 4, 4, 3, null], "2026-10-28")} format={count} />,
    );
    const plot = screen.getByRole("application", { name: "Visitors" });
    expect(plot.tabIndex).toBe(0);
    const readout = screen.getByRole("status");
    expect(readout.textContent).toBe("");

    // Focus starts on the latest bucket: the one a reader most wants.
    act(() => plot.focus());
    expect(readout.textContent).toBe("Nov 6: 6; Previous not measured");
    fireEvent.keyDown(plot, { key: "ArrowLeft" });
    expect(readout.textContent).toBe("Nov 5: 4; Previous 3");
    fireEvent.keyDown(plot, { key: "ArrowLeft" });
    expect(readout.textContent).toBe("Nov 4: Not measured; Previous 4");
    fireEvent.keyDown(plot, { key: "Home" });
    expect(readout.textContent).toBe("Nov 2: 3; Previous 2");
    // The ends hold rather than wrapping round.
    fireEvent.keyDown(plot, { key: "ArrowLeft" });
    expect(readout.textContent).toBe("Nov 2: 3; Previous 2");
    fireEvent.keyDown(plot, { key: "ArrowRight" });
    expect(readout.textContent).toBe("Nov 3: 5; Previous 4");
    fireEvent.keyDown(plot, { key: "End" });
    expect(readout.textContent).toBe("Nov 6: 6; Previous not measured");

    act(() => plot.blur());
    expect(readout.textContent).toBe("");
  });

  it("follows the pointer to the nearest bucket and lets go when it leaves", () => {
    render(<TimeChart label="Visitors" bucket="day" points={days([3, 5, 9, 4, 6])} format={count} />);
    const plot = screen.getByRole("application", { name: "Visitors" });
    plot.getBoundingClientRect = () => ({ left: 100, top: 0, right: 500, bottom: 160, width: 400, height: 160, x: 100, y: 0, toJSON: () => ({}) });
    const readout = screen.getByRole("status");

    // 210 px is just past a quarter of the way across: the second bucket.
    fireEvent.pointerMove(plot, { clientX: 210 });
    expect(readout.textContent).toBe("Nov 3: 5");
    fireEvent.pointerMove(plot, { clientX: 300 });
    expect(readout.textContent).toBe("Nov 4: 9");
    expect(document.querySelector(".os-time-chart-guide")).not.toBeNull();
    fireEvent.pointerLeave(plot);
    expect(readout.textContent).toBe("");
    expect(document.querySelector(".os-time-chart-guide")).toBeNull();
  });

  it("holds its place when the series gets shorter under it", () => {
    const { rerender } = render(<TimeChart label="Visitors" bucket="day" points={days([3, 5, 9, 4, 6])} format={count} />);
    const plot = screen.getByRole("application", { name: "Visitors" });
    act(() => plot.focus());
    rerender(<TimeChart label="Visitors" bucket="day" points={days([3, 5])} format={count} />);
    expect(screen.getByRole("status").textContent).toBe("Nov 3: 5");
  });
});

describe("for a screen reader", () => {
  it("the summary names the highest and the unmeasured", () => {
    const { container } = render(<TimeChart label="Visitors" bucket="day" points={days([3, 1204, null, 4, null])} format={count} />);
    const caption = container.querySelector("figure > figcaption");
    expect(caption?.classList.contains("os-sr-only")).toBe(true);
    expect(caption?.textContent).toBe("Visitors: 5 points from Nov 2 to Nov 6; highest 1,204 on Nov 3; 2 not measured.");
  });

  it("says nothing about the unmeasured when every point was measured, and no highest when none was", () => {
    const { container, rerender } = render(<TimeChart label="Visitors" bucket="day" points={days([3, 9, 4])} format={count} />);
    expect(container.querySelector("figcaption")?.textContent).toBe("Visitors: 3 points from Nov 2 to Nov 4; highest 9 on Nov 3.");
    rerender(<TimeChart label="Visitors" bucket="day" points={days([null, null])} format={count} />);
    expect(container.querySelector("figcaption")?.textContent).toBe("Visitors: 2 points from Nov 2 to Nov 3; 2 not measured.");
    rerender(<TimeChart label="Visitors" bucket="day" points={days([7])} format={count} />);
    expect(container.querySelector("figcaption")?.textContent).toBe("Visitors: 1 point on Nov 2; highest 7 on Nov 2.");
    rerender(<TimeChart label="Visitors" bucket="day" points={[]} format={count} />);
    expect(container.querySelector("figcaption")?.textContent).toBe("Visitors: no points.");
  });
});

describe("its colours and motion", () => {
  it("come from the theme's tokens, so light and dark both hold", () => {
    expect(CSS).toMatch(/\.os-time-chart-line \{[^}]*stroke: var\(--os-accent\);/);
    expect(CSS).toMatch(/\.os-time-chart-area \{[^}]*fill: var\(--os-accent-soft\);/);
    expect(CSS).toMatch(/\.os-time-chart-previous \{[^}]*stroke: var\(--os-muted\);/);
    expect(CSS).not.toMatch(/#[0-9a-fA-F]{3,8}\b/);
  });

  it("stops under reduced motion", () => {
    const reduced = CSS.slice(CSS.indexOf("@media (prefers-reduced-motion: reduce)"));
    expect(reduced).toMatch(/\.os-time-chart-guide,\s*\.os-time-chart-dot \{ transition: none; \}/);
  });
});
