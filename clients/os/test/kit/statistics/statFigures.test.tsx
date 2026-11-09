import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { absent, figureOf } from "../../../src/kit/measure";
import { StatFigures, type StatFigure } from "../../../src/kit/statistics/StatFigures";

afterEach(cleanup);

// THE FIGURE ROW (spec 8). Each figure is its value, its change from the
// previous period and its label; the ones the series can draw are the options
// of one radiogroup that decides what the chart below draws. A figure the
// series cannot draw (an average with no per-bucket value) is still shown, as
// a plain figure, because a control that does nothing is not a control.

const CSS = readFileSync(join(__dirname, "..", "..", "..", "src", "kit", "statistics", "statistics.css"), "utf8");
const percent = (value: number) => `${Math.round(value * 100)}%`;

const FIGURES: StatFigure[] = [
  { id: "visitors", label: "Visitors", figure: figureOf(1204), previous: figureOf(1075), chartable: true },
  { id: "visits", label: "Visits", figure: figureOf(1530), previous: figureOf(1530), chartable: true },
  { id: "views", label: "Page views", figure: figureOf(4210), previous: figureOf(0), chartable: true },
  { id: "perVisit", label: "Views per visit", figure: figureOf(2.75), format: (value) => value.toFixed(1) },
  { id: "bounce", label: "Bounce rate", figure: absent("unmeasured"), previous: figureOf(0.4), format: percent, chartable: true },
];

function labelsOf(elements: HTMLElement[]): (string | null | undefined)[] {
  return elements.map((element) => element.querySelector(".os-stat-figure-label")?.textContent);
}

function changeIn(element: HTMLElement): string | null | undefined {
  return element.querySelector(".os-stat-figure-change")?.textContent;
}

describe("the figure row", () => {
  it("only chartable figures are radios", () => {
    render(<StatFigures label="Website figures" figures={FIGURES} selected="visitors" onSelect={() => {}} />);
    const group = screen.getByRole("radiogroup", { name: "Website figures" });
    const radios = within(group).getAllByRole("radio");
    expect(labelsOf(radios)).toEqual(["Visitors", "Visits", "Page views", "Bounce rate"]);
    // The figure the series cannot draw is still there, in its place, as a
    // plain figure rather than a control that does nothing.
    const plain = within(group).getByText("Views per visit").closest(".os-stat-figure") as HTMLElement;
    expect(plain.tagName).toBe("DIV");
    expect(plain.getAttribute("role")).toBeNull();
    expect(plain.textContent).toContain("2.8");
    expect([...group.querySelectorAll(".os-stat-figure-label")].map((label) => label.textContent)).toEqual([
      "Visitors", "Visits", "Page views", "Views per visit", "Bounce rate",
    ]);
    // The selection is the checked radio, and only it stands in the tab order.
    expect(radios.map((radio) => radio.getAttribute("aria-checked"))).toEqual(["true", "false", "false", "false"]);
    expect(radios.map((radio) => radio.tabIndex)).toEqual([0, -1, -1, -1]);
  });

  it("is a plain row of figures when nothing listens for a choice", () => {
    render(<StatFigures label="Health" figures={FIGURES} />);
    expect(screen.queryByRole("radio")).toBeNull();
    expect(screen.queryByRole("radiogroup")).toBeNull();
    expect(screen.getByRole("group", { name: "Health" }).querySelectorAll(".os-stat-figure")).toHaveLength(5);
  });

  it("puts the first chartable figure in the tab order when none is selected", () => {
    render(<StatFigures label="Website figures" figures={FIGURES} selected="perVisit" onSelect={() => {}} />);
    const radios = screen.getAllByRole("radio");
    expect(radios.map((radio) => radio.getAttribute("aria-checked"))).toEqual(["false", "false", "false", "false"]);
    expect(radios.map((radio) => radio.tabIndex)).toEqual([0, -1, -1, -1]);
  });

  it("arrow keys change the selected figure", () => {
    const onSelect = vi.fn();
    const { rerender } = render(<StatFigures label="Website figures" figures={FIGURES} selected="visitors" onSelect={onSelect} />);
    const radios = () => screen.getAllByRole("radio");

    act(() => radios()[0]!.focus());
    fireEvent.keyDown(radios()[0]!, { key: "ArrowRight" });
    expect(onSelect).toHaveBeenLastCalledWith("visits");
    expect(document.activeElement).toBe(radios()[1]);

    // The plain figure between Page views and Bounce rate is not a stop.
    rerender(<StatFigures label="Website figures" figures={FIGURES} selected="views" onSelect={onSelect} />);
    fireEvent.keyDown(radios()[2]!, { key: "ArrowDown" });
    expect(onSelect).toHaveBeenLastCalledWith("bounce");
    expect(document.activeElement).toBe(radios()[3]);

    // Round from the last to the first, and back.
    rerender(<StatFigures label="Website figures" figures={FIGURES} selected="bounce" onSelect={onSelect} />);
    fireEvent.keyDown(radios()[3]!, { key: "ArrowRight" });
    expect(onSelect).toHaveBeenLastCalledWith("visitors");
    fireEvent.keyDown(radios()[0]!, { key: "ArrowLeft" });
    expect(onSelect).toHaveBeenLastCalledWith("bounce");
    fireEvent.keyDown(radios()[2]!, { key: "Home" });
    expect(onSelect).toHaveBeenLastCalledWith("visitors");
    fireEvent.keyDown(radios()[0]!, { key: "End" });
    expect(onSelect).toHaveBeenLastCalledWith("bounce");

    // And a press chooses, the way a pointer does.
    fireEvent.click(radios()[2]!);
    expect(onSelect).toHaveBeenLastCalledWith("views");
  });
});

describe("a figure's change", () => {
  it("change reads against the previous period", () => {
    render(<StatFigures label="Website figures" figures={FIGURES} selected="visitors" onSelect={() => {}} />);
    const [visitors, visits, views] = screen.getAllByRole("radio");
    expect(visitors!.querySelector(".os-stat-figure-value")?.textContent).toBe("1,204");
    expect(changeIn(visitors!)).toBe("+12%");
    expect(changeIn(visits!)).toBe("No change");
    expect(changeIn(views!)).toBe("New");
  });

  it("an absent previous shows no change", () => {
    const figures: StatFigure[] = [
      { id: "visitors", label: "Visitors", figure: figureOf(1204), previous: absent("unmeasured"), chartable: true },
      // Twelve months: no previous period at all.
      { id: "visits", label: "Visits", figure: figureOf(1530), chartable: true },
      // Nothing measured now, something before: still no change, and a dash.
      { id: "bounce", label: "Bounce rate", figure: absent("unmeasured"), previous: figureOf(0.4), format: percent, chartable: true },
    ];
    render(<StatFigures label="Website figures" figures={figures} selected="visitors" onSelect={() => {}} />);
    const radios = screen.getAllByRole("radio");
    expect(radios.map(changeIn)).toEqual(["", "", ""]);
    expect(radios[2]!.querySelector(".os-stat-figure-value")?.textContent).toBe("—");
  });

  it("is the shape of a figure while it is read, with no change beside it", () => {
    // Whatever figure is held while the read is out, neither it nor a change
    // computed from it is drawn.
    render(<StatFigures label="Website figures" figures={[{ id: "visitors", label: "Visitors", figure: figureOf(5), previous: figureOf(4), chartable: true, loading: true }]} selected="visitors" onSelect={() => {}} />);
    const radio = screen.getByRole("radio");
    const shape = within(radio).getByRole("status");
    expect(shape.getAttribute("aria-busy")).toBe("true");
    expect(shape.textContent).toBe("Loading visitors");
    expect(changeIn(radio)).toBe("");
  });
});

describe("its layout", () => {
  it("stands four in a row then two, and five in a row then three then two", () => {
    const { container, rerender } = render(<StatFigures label="Website figures" figures={FIGURES} />);
    expect(container.querySelector(".os-stat-figures-grid")?.getAttribute("data-count")).toBe("5");
    rerender(<StatFigures label="Health" figures={FIGURES.slice(0, 4)} />);
    expect(container.querySelector(".os-stat-figures-grid")?.getAttribute("data-count")).toBe("4");

    expect(CSS).toMatch(/\.os-stat-figures \{[^}]*container: stat-figures \/ inline-size;/);
    expect(CSS).toMatch(/\.os-stat-figures-grid \{[^}]*grid-template-columns: repeat\(4, minmax\(0, 1fr\)\);/);
    expect(CSS).toMatch(/\.os-stat-figures-grid\[data-count="5"\] \{ grid-template-columns: repeat\(5, minmax\(0, 1fr\)\); \}/);
    expect(CSS).toMatch(/@container stat-figures \(max-width: 760px\) \{ \.os-stat-figures-grid\[data-count="5"\] \{ grid-template-columns: repeat\(3, minmax\(0, 1fr\)\); \} \}/);
    expect(CSS).toMatch(/@container stat-figures \(max-width: 420px\) \{ \.os-stat-figures-grid, \.os-stat-figures-grid\[data-count="5"\] \{ grid-template-columns: repeat\(2, minmax\(0, 1fr\)\); \} \}/);
  });

  it("marks the selected figure with the accent, on straight edges", () => {
    expect(CSS).toMatch(/\.os-stat-figure\[aria-checked="true"\] \{[^}]*var\(--os-accent\)/);
    expect(CSS).toMatch(/\.os-stat-figure \{[^}]*border-radius: var\(--os-selection-radius\);/);
  });
});
