// The map's connectors ARRIVE at their nodes; they never cross them
// (znasllc-io/memql#5068).
//
// # The failure this exists to prevent
//
// Every shape on the road is `fill: none` -- an open ring is the map saying "a
// position", not "a thing". So a connector drawn centre to centre is not hidden
// by the glyph it runs into: it is VISIBLE INSIDE it. Across the beacon, 44px
// wide and the one element this surface is allowed to be loud with, the road
// ran through the ring and stopped at the core, which reads as a line crossing
// the goal out rather than a road arriving at it.
//
// # Why it is asserted this way
//
// The property is geometric, so it is checked geometrically: no connector
// endpoint may lie within the glyph it points at. Asserting the trimmed
// coordinates directly would just restate the arithmetic in the component and
// would still pass if the standoff were wired to the wrong node.

import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";

import { BeaconMap } from "../../src/apps/nexus/BeaconMap";
import { chainWorld, fanOutWorld } from "../../src/nexus/scene/fixtures";
import type { GoalWorld } from "../../src/nexus/scene/world";

function draw(world: GoalWorld, selectStep = (_key: string) => {}): SVGSVGElement {
  const { container } = render(
    <BeaconMap
      world={world}
      state="live"
      selectedStepKey=""
      onSelectStep={selectStep}
      onOpenApproval={() => {}}
      expandedColumns={new Set()}
      expandedFolds={new Set()}
      onToggleColumn={() => {}}
      onToggleFold={() => {}}
      at=""
    />,
  );
  const svg = container.querySelector("svg");
  if (svg === null) throw new Error("the map drew no canvas");
  return svg as SVGSVGElement;
}

interface Disc {
  what: string;
  cx: number;
  cy: number;
  r: number;
}

/** Every circular glyph on the canvas, with the radius it actually draws at. */
function discs(svg: SVGSVGElement): Disc[] {
  const out: Disc[] = [];
  for (const el of svg.querySelectorAll("circle")) {
    const cls = el.getAttribute("class") ?? "";
    // The progress arc and the beacon's core sit inside the ring; the ring is
    // the outer boundary and the only one a connector has to clear.
    if (cls.includes("beacon-fill") || cls.includes("beacon-core")) continue;
    out.push({
      what: cls,
      cx: Number(el.getAttribute("cx")),
      cy: Number(el.getAttribute("cy")),
      r: Number(el.getAttribute("r")),
    });
  }
  return out;
}

interface Seg {
  what: string;
  x1: number;
  y1: number;
  x2: number;
  y2: number;
}

function segments(svg: SVGSVGElement): Seg[] {
  return [...svg.querySelectorAll("line")].map((el) => ({
    what: el.getAttribute("class") ?? "",
    x1: Number(el.getAttribute("x1")),
    y1: Number(el.getAttribute("y1")),
    x2: Number(el.getAttribute("x2")),
    y2: Number(el.getAttribute("y2")),
  }));
}

function hypot(ax: number, ay: number, bx: number, by: number): number {
  return Math.hypot(ax - bx, ay - by);
}

function assertNoEndpointInsideAGlyph(world: GoalWorld, name: string) {
  const svg = draw(world);
  const glyphs = discs(svg);
  const lines = segments(svg);

  expect(lines.length, `${name}: the map drew no connectors`).toBeGreaterThan(0);
  expect(glyphs.length, `${name}: the map drew no glyphs`).toBeGreaterThan(0);

  for (const seg of lines) {
    for (const g of glyphs) {
      for (const [ex, ey, end] of [
        [seg.x1, seg.y1, "start"],
        [seg.x2, seg.y2, "end"],
      ] as const) {
        const d = hypot(ex, ey, g.cx, g.cy);
        expect(
          d,
          `${name}: the ${end} of a ${seg.what} lands ${d.toFixed(1)}px from the centre of ` +
            `${g.what} (r=${g.r}) -- inside it, so the line is drawn through the glyph`,
        ).toBeGreaterThan(g.r);
      }
    }
  }
}

describe("connectors arrive at their nodes", () => {
  it("leaves the goal beacon clear on a straight run", () => {
    assertNoEndpointInsideAGlyph(chainWorld(5), "chain");
  });

  it("leaves every glyph clear when steps fan out and rejoin", () => {
    // The fan is where the DIAGONAL dependency edges exist. They run into the
    // same unfilled circles the road does, so fixing only the road would have
    // left the identical artefact a few pixels away.
    assertNoEndpointInsideAGlyph(fanOutWorld(3), "fan-out");
  });

  it("clears the beacon by the arrival gap, not by a hair", () => {
    // A standoff of exactly the radius would satisfy the checks above and still
    // look like the line is touching the ring. The gap is the point.
    const svg = draw(chainWorld(5));
    const ring = discs(svg).find((g) => g.what.includes("beacon-ring"));
    expect(ring, "no beacon ring on the canvas").toBeDefined();

    const nearest = Math.min(
      ...segments(svg).flatMap((s) => [
        hypot(s.x1, s.y1, ring!.cx, ring!.cy),
        hypot(s.x2, s.y2, ring!.cx, ring!.cy),
      ]),
    );
    // Ring radius 22, stroke 3 (so it paints out to 23.5), plus clear space.
    expect(nearest).toBeGreaterThan(ring!.r + 3);
  });
});

describe("the ends of the road", () => {
  it("calls the near end the start of the work, not the person", () => {
    const svg = draw(chainWorld(3));
    const labels = [...svg.querySelectorAll("text")].map((t) => t.textContent);
    expect(labels).toContain("start");
    expect(labels).not.toContain("you");
  });
});

describe("steering the beacon map", () => {
  it("leaves scrolling alone and offers the shared zoom controls", () => {
    const svg = draw(chainWorld(5));
    const transform = () => svg.querySelector("[data-os-map-view]")!.getAttribute("transform");
    const initial = transform();
    const wheel = new WheelEvent("wheel", { deltaY: -120, bubbles: true, cancelable: true });
    fireEvent(svg, wheel);
    expect(wheel.defaultPrevented).toBe(false);
    expect(transform()).toBe(initial);
    fireEvent.click(screen.getByRole("button", { name: "Zoom in" }));
    expect(transform()).not.toBe(initial);
    fireEvent.click(screen.getByRole("button", { name: "Reset zoom and position" }));
    expect(transform()).toBe(initial);
  });

  it("does not open a dragged step, but still opens it from the keyboard afterwards", () => {
    const select = vi.fn();
    const svg = draw(chainWorld(5, "running"), select);
    const step = svg.querySelector('[data-kind="step"]')!;
    expect(step).not.toBeNull();
    fireEvent.pointerDown(step, { pointerId: 1, clientX: 10, clientY: 10 });
    fireEvent.pointerMove(svg, { pointerId: 1, clientX: 80, clientY: 10 });
    fireEvent.pointerUp(svg, { pointerId: 1, clientX: 80, clientY: 10 });
    fireEvent.click(step, { detail: 1 });
    expect(select).not.toHaveBeenCalled();
    fireEvent.keyDown(step, { key: "Enter" });
    expect(select).toHaveBeenCalledTimes(1);
  });
});
