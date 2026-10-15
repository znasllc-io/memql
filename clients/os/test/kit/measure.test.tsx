import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { absent, countFigure, figureOf, figureValue } from "../../src/kit/measure";
import { Measure } from "../../src/kit/MeasureView";

// A COUNT THAT STOPPED AT THE ENGINE'S WINDOW IS A FLOOR (memql#6033).
//
// The engine reads a count up to its window (5,000 by default, configurable)
// and says when the set was larger: the figure is then the window, and "this
// many or more" is the only true way to say it. A floor is still a
// measurement -- somebody counted -- but never an exact one, so it is drawn
// with "or more" wherever it is drawn.

afterEach(cleanup);

const count = (value: number) => value.toLocaleString("en-US");

describe("a count's figure", () => {
  it("is a floor when the count says it stopped at the window, and exact otherwise", () => {
    expect(countFigure({ count: 5000, atLeast: true })).toEqual({ kind: "measured", value: 5000, atLeast: true });
    expect(countFigure({ count: 1204, atLeast: false })).toEqual(figureOf(1204));
    expect(countFigure({ count: 0, atLeast: false })).toEqual(figureOf(0));
    // No count at all is not a zero.
    expect(countFigure(null)).toEqual(absent("unmeasured"));
  });

  it("keeps its number for a caller that branches on it", () => {
    expect(figureValue(figureOf(5000, true))).toBe(5000);
  });
});

describe("a floor, drawn", () => {
  it("reads 'or more', in the figure's own format", () => {
    const { container } = render(<Measure figure={figureOf(5000, true)} format={count} />);
    expect(container.textContent).toBe("5,000 or more");
    // Beside the number as quietly as a unit, in one piece.
    expect(container.querySelector(".os-figure-floor")?.textContent).toBe(" or more");
  });

  it("follows its unit, and an exact count reads as it is", () => {
    const { container, rerender } = render(<Measure figure={figureOf(5000, true)} format={count} suffix=" rows" />);
    expect(container.textContent).toBe("5,000 rows or more");
    rerender(<Measure figure={figureOf(1204)} format={count} />);
    expect(container.textContent).toBe("1,204");
  });
});
