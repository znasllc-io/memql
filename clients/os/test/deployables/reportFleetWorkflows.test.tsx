import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { ReportView } from "../../src/apps/deployables/packages/ReportView";
import type { AnalysisReport } from "../../src/apps/deployables/packages/rows";

// A workflow declaring @usesDeployerFleet runs its model calls on whoever presses
// Deploy, so the review says so before they do.

function reportWith(fleetWorkflows?: string[]): AnalysisReport {
  return {
    deployables: [],
    dslDomains: [{ domain: "shop", constructs: { automation: 1 }, files: 1, ...(fleetWorkflows ? { fleetWorkflows } : {}) }],
  } as unknown as AnalysisReport;
}

describe("the deploy review", () => {
  it("names the workflows that will use the deployer's machines", () => {
    render(<ReportView report={reportWith(["writeStatusBrief", "zebraBrief"])} />);
    expect(screen.getByText("These workflows will use your machines: writeStatusBrief, zebraBrief.")).toBeTruthy();
  });

  it("says nothing about machines when no workflow declared it", () => {
    render(<ReportView report={reportWith()} />);
    expect(screen.queryByText(/will use your machines/)).toBeNull();
  });
});
