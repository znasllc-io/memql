// The five layouts, rendered (epic memql#4661, task memql#4664).
//
// The claim under test is narrow: given a sanitized arrangement, every entry
// reaches the DOM, in the slot view-kit's planner assigned it, with the CSS
// class the stylesheet styles. It is a rendering test rather than a layout
// test -- jsdom has no layout engine and will happily report every element as
// 0x0, so what a grid actually LOOKS like is verified in the visual QA sweep
// (memql#4675) against a real browser, and nothing here pretends otherwise.
//
// What jsdom CAN answer, and what nothing else answers as cheaply, is
// "did anything get lost". That is the failure mode a layout introduces
// silently: a page missing an element still looks deliberate.

import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";
import {
  SECTION_LAYOUTS,
  type Arrangement,
  type ConceptLike,
  type RowLike,
  type SectionLayout,
} from "@znasllc-io/memql-view-kit";

import { ArrangementLayout } from "../src/compose/ArrangementLayout";

const CONCEPT: ConceptLike = {
  id: "v9:madeup:sensorReading",
  entity: "sensorReading",
  displayCard: { primary: "label", secondary: "zone", status: "faulty" },
};

const ROWS: readonly RowLike[] = [
  { id: "r1", label: "north inlet", zone: "intake", degrees: 41.2, takenAt: "2026-08-01T06:00:00Z", faulty: false },
  { id: "r2", label: "south inlet", zone: "intake", degrees: 39.8, takenAt: "2026-08-01T07:00:00Z", faulty: false },
  { id: "r3", label: "return line", zone: "return", degrees: 55.4, takenAt: "2026-08-01T08:00:00Z", faulty: true },
];

function arrange(layout: SectionLayout | undefined, elements: Arrangement["elements"]): Arrangement {
  return {
    conceptId: CONCEPT.id,
    ...(layout === undefined ? {} : { layout }),
    elements,
  };
}

const MIXED: Arrangement["elements"] = [
  { element: "statTile", band: "reading" },
  { element: "chart.proportion", band: "shape" },
  { element: "table", band: "roll" },
  { element: "detail", band: "roll" },
];

describe("the arrangement renderer", () => {
  it("renders every entry of every layout, losing none", () => {
    for (const layout of SECTION_LAYOUTS) {
      const { container, unmount } = render(
        <ArrangementLayout arrangement={arrange(layout, MIXED)} concept={CONCEPT} rows={ROWS} />,
      );
      const slots = container.querySelectorAll(".vk-slot");
      expect(slots.length, `${layout} rendered no slots`).toBeGreaterThan(0);
      // Each entry gets exactly one role wrapper, and every layout renders
      // all four. A dropped entry is the failure this whole file is for.
      const wrappers = container.querySelectorAll(
        ".vk-role-standard, .vk-role-hero, .vk-role-supporting",
      );
      expect(wrappers.length, `${layout} rendered ${wrappers.length} of 4 entries`).toBe(4);
      unmount();
    }
  });

  it("stack is what an absent layout means, and it is one column", () => {
    // The fallback every repair lands on: a regression here is a regression
    // in every view stored before layouts existed.
    const { container } = render(
      <ArrangementLayout arrangement={arrange(undefined, MIXED)} concept={CONCEPT} rows={ROWS} />,
    );
    expect(container.querySelector(".vk-arrangement-stack")).not.toBeNull();
    const slots = container.querySelectorAll(".vk-slot");
    expect(slots.length).toBe(1);
    expect(slots[0]?.classList.contains("vk-slot-flow")).toBe(true);
  });

  it("puts the split's detail pane in the detail slot and the table in the roll", () => {
    const { container } = render(
      <ArrangementLayout arrangement={arrange("split", MIXED)} concept={CONCEPT} rows={ROWS} />,
    );
    expect(container.querySelector(".vk-arrangement-split")).not.toBeNull();
    expect(container.querySelector(".vk-slot-detail")).not.toBeNull();
    expect(container.querySelector(".vk-slot-roll .vk-table")).not.toBeNull();
  });

  it("marks the focus hero with the emphasis class the stylesheet styles", () => {
    const { container } = render(
      <ArrangementLayout
        arrangement={arrange("focus", [
          { element: "chart.proportion", band: "shape", role: "hero" },
          { element: "table", band: "roll", role: "supporting" },
        ])}
        concept={CONCEPT}
        rows={ROWS}
      />,
    );
    const lead = container.querySelector(".vk-slot-lead");
    expect(lead?.querySelector(".vk-role-hero")).not.toBeNull();
    expect(container.querySelector(".vk-slot-aside .vk-role-supporting")).not.toBeNull();
  });

  it("says so out loud when an element is missing from this build", () => {
    // A saved view is durable and the element library is not. Rendering one
    // band fewer and looking fine is the outcome this refuses.
    const { getByText } = render(
      <ArrangementLayout
        arrangement={arrange(undefined, [{ element: "sparklineFromTheFuture", band: "roll" }])}
        concept={CONCEPT}
        rows={ROWS}
      />,
    );
    expect(getByText(/does not have/)).toBeTruthy();
  });

  it("lets the host render a scene or widget instead of view-kit's placeholder", () => {
    // view-kit's own render for a hosted kind says "this surface does not
    // render scenes", which is correct for a host that has none and
    // misleading on one that does.
    const { getByText, queryByText } = render(
      <ArrangementLayout
        arrangement={arrange(undefined, [
          { element: "scene", band: "shape", options: { sceneId: "conceptGraph" } },
        ])}
        concept={CONCEPT}
        rows={ROWS}
        renderModule={(planned) => <div>hosted:{planned.entry.options?.["sceneId"]}</div>}
      />,
    );
    expect(getByText("hosted:conceptGraph")).toBeTruthy();
    expect(queryByText(/does not render scenes/)).toBeNull();
  });

  it("renders nothing but an invitation when the section is empty", () => {
    const { getByText, container } = render(
      <ArrangementLayout arrangement={arrange("dashboard", [])} concept={CONCEPT} rows={ROWS} />,
    );
    expect(getByText(/no elements yet/)).toBeTruthy();
    expect(container.querySelector(".vk-arrangement")).toBeNull();
  });
});
