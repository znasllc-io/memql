import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { figureOf } from "../../src/kit/measure";
import { Overview } from "../../src/kit/Overview";
import { StatFigures } from "../../src/kit/statistics/StatFigures";

afterEach(cleanup);

// An app whose Overview is its statistics (Email, spec 9) draws its figure
// row with the statistics kit, in the place the Overview's own metrics stand:
// one row of figures under the heading, not two.

describe("the overview", () => {
  it("overview draws figures in place of metrics", () => {
    render(
      <Overview
        metrics={[{ label: "Live campaigns", figure: figureOf(3) }]}
        figures={<StatFigures label="Email figures" figures={[{ id: "sent", label: "Sent", figure: figureOf(1204), chartable: true }]} selected="sent" onSelect={() => {}} />}
      >
        <p>Below the figures</p>
      </Overview>,
    );
    expect(document.querySelector(".os-overview-metrics")).toBeNull();
    expect(screen.queryByText("Live campaigns")).toBeNull();
    const figures = screen.getByRole("radiogroup", { name: "Email figures" });
    // Under the heading and above the section's own content.
    const heading = screen.getByRole("heading", { name: "Overview" });
    expect(heading.compareDocumentPosition(figures) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(figures.compareDocumentPosition(screen.getByText("Below the figures")) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("keeps its own metrics when it is given no figures", () => {
    render(<Overview metrics={[{ label: "Live campaigns", figure: figureOf(3) }]}>{null}</Overview>);
    expect(document.querySelector("dl.os-overview-metrics")).not.toBeNull();
    expect(screen.getByText("Live campaigns")).toBeTruthy();
  });
});
