// The Constellation: the product's one expressive element (decision D4).
//
// What a unit test can own here is narrow and worth owning: that the geometry
// is the mark's and not a second trace, that reduced motion produces a
// DIFFERENT TREE rather than relying on a stylesheet jsdom never applies, and
// that the sizes change weights without changing positions.
//
// What it deliberately does not own is what the assemble LOOKS like. That is
// the visual QA pass's job (memql#4660), and asserting on a keyframe name here
// would be asserting that a string is spelled the way this test spells it.

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import { Constellation } from "../src/ui/Constellation";
import { EmptyState } from "../src/ui/EmptyState";
import { MemqlMark } from "../src/components/MemqlMark";
import { MARK_EDGES, MARK_NODES } from "../src/ui/markGeometry";

// jsdom implements no matchMedia at all, so BOTH states have to be stubbed --
// including "the person has not asked for reduced motion", which is otherwise
// indistinguishable from "the stub is missing".
function stubReducedMotion(reduced: boolean): void {
  Object.defineProperty(window, "matchMedia", {
    writable: true,
    configurable: true,
    value: (query: string) => ({
      matches: reduced && query.includes("prefers-reduced-motion"),
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }),
  });
}

function root(): HTMLElement {
  const el = document.querySelector(".constellation");
  if (el === null) throw new Error("no .constellation rendered");
  return el as HTMLElement;
}

afterEach(() => {
  cleanup();
  Reflect.deleteProperty(window, "matchMedia");
});

describe("the Constellation", () => {
  it("draws the mark's own nine nodes and nineteen edges", () => {
    stubReducedMotion(false);
    render(<Constellation />);
    expect(root().querySelectorAll("circle")).toHaveLength(MARK_NODES.length);
    expect(root().querySelectorAll("line")).toHaveLength(MARK_EDGES.length);
    expect(MARK_NODES).toHaveLength(9);
    expect(MARK_EDGES).toHaveLength(19);
  });

  it("shares its node POSITIONS with the rail mark, exactly", () => {
    // The regression this guards is a second trace: two renderers of one mark
    // whose coordinates drifted a decimal apart, which nobody sees until the
    // two are on screen together.
    stubReducedMotion(false);
    const { container: markContainer } = render(<MemqlMark />);
    const markPositions = [...markContainer.querySelectorAll("circle")].map(
      (c) => `${c.getAttribute("cx")},${c.getAttribute("cy")}`,
    );
    cleanup();
    render(<Constellation size="lg" />);
    const constellationPositions = [...root().querySelectorAll("circle")].map(
      (c) => `${c.getAttribute("cx")},${c.getAttribute("cy")}`,
    );
    expect(constellationPositions).toEqual(markPositions);
  });

  it("changes WEIGHTS with size, never positions", () => {
    stubReducedMotion(false);
    render(<Constellation size="sm" />);
    const smallR = root().querySelector("circle")?.getAttribute("r");
    const smallBox = root().querySelector("svg")?.getAttribute("width");
    cleanup();
    render(<Constellation size="lg" />);
    const largeR = root().querySelector("circle")?.getAttribute("r");
    const largeBox = root().querySelector("svg")?.getAttribute("width");

    expect(Number(largeBox)).toBeGreaterThan(Number(smallBox));
    // The dot gets proportionally SMALLER as the box grows -- that is what
    // turns an icon into a constellation.
    expect(Number(largeR)).toBeLessThan(Number(smallR));
  });

  it("assembles once by default", () => {
    stubReducedMotion(false);
    render(<Constellation />);
    expect(root().getAttribute("data-animate")).toBe("true");
  });

  it("does not assemble when the caller says not to", () => {
    stubReducedMotion(false);
    render(<Constellation animate={false} />);
    expect(root().getAttribute("data-animate")).toBe("false");
  });

  it("renders the final frame statically under reduced motion", () => {
    stubReducedMotion(true);
    render(<Constellation />);
    // The flag is off even though the caller asked to animate: the preference
    // outranks the prop, which is the direction that cannot be got wrong.
    expect(root().getAttribute("data-animate")).toBe("false");
    // ...and the mark is fully drawn, not a blank box waiting for frames.
    expect(root().querySelectorAll("circle")).toHaveLength(9);
    expect(root().querySelectorAll("line")).toHaveLength(19);
  });

  it("is decoration, and says so", () => {
    stubReducedMotion(false);
    render(<Constellation />);
    expect(root().querySelector("svg")?.getAttribute("aria-hidden")).toBe("true");
  });
});

describe("EmptyState's first-run flag", () => {
  it("stays exactly as it was by default", () => {
    stubReducedMotion(false);
    render(<EmptyState statement="No results match that filter." />);
    expect(document.querySelector(".constellation")).toBeNull();
    expect(screen.getByText("No results match that filter.")).toBeTruthy();
  });

  it("carries the mark when the emptiness is a first run", () => {
    stubReducedMotion(false);
    render(<EmptyState firstRun statement="You have no machines yet." />);
    expect(document.querySelector(".constellation")).toBeTruthy();
  });

  it("lets an explicit icon win, so one sentence never gets two pictures", () => {
    stubReducedMotion(false);
    render(<EmptyState firstRun icon={<span data-testid="chosen" />} statement="Nothing here." />);
    expect(screen.getByTestId("chosen")).toBeTruthy();
    expect(document.querySelector(".constellation")).toBeNull();
  });
});
