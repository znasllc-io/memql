import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { ReportView } from "../../src/apps/deployables/packages/ReportView";
import type { AnalysisReport } from "../../src/apps/deployables/packages/rows";

// The report says each thing ONCE (DESIGN.md rule 7).
//
// `rep.add` records a per-deployable problem on the report AND on its own
// deployable, so a non-fatal one is in two places in the data. Until
// `deployable_target_not_offered` arrived (epic memql#4885) the only non-fatal
// problem was the Go pack's, which the foot of the report suppressed by hand,
// so the double never showed. It showed the moment a second non-fatal class
// existed: "iOS is not offered on this cluster yet" rendered inside the app's
// card and again under the MemQL heading, which is about DSL domains.

afterEach(cleanup);

const NOT_OFFERED = {
  code: "deployable_target_not_offered",
  message: "iOS is not offered on this cluster yet",
  scope: "mobile",
  fatal: false,
};

function report(over: Partial<AnalysisReport> = {}): AnalysisReport {
  return {
    name: "acme",
    deployables: [
      { name: "docs", kind: "static", path: "clients/docs", buildPlan: "prebuilt output found -- build skipped", output: "dist", prebuilt: true },
      { name: "mobile", kind: "ios", path: "clients/mobile", buildPlan: "not analyzed", output: "dist", prebuilt: false, problem: NOT_OFFERED },
    ],
    dslDomains: [],
    problems: [NOT_OFFERED],
    ok: true,
    ...over,
  };
}

describe("the analysis report", () => {
  it("says a not-offered target ONCE, on the app it is about", () => {
    render(<ReportView report={report()} />);
    // The server's own sentence, and exactly one of it.
    expect(screen.getAllByText("iOS is not offered on this cluster yet")).toHaveLength(1);
    // And it is beside the app, not at the foot of the report: the heading it
    // used to land under is about MemQL domains.
    const shown = screen.getByText("iOS is not offered on this cluster yet");
    expect(shown.closest("li")?.textContent).toContain("mobile");
  });

  it("still shows a non-fatal problem that belongs to NO app", () => {
    // The reachable positive. Without it, the filter could suppress every note
    // and the test above would pass for the wrong reason -- a report whose
    // package-wide notes had silently stopped rendering.
    const packageWide = { code: "go_pack_not_deployable", message: "reported, not deployable through this path", scope: "", fatal: false };
    render(<ReportView report={report({ problems: [NOT_OFFERED, packageWide] })} />);
    expect(screen.getByText(/reported, not deployable through this path/)).toBeTruthy();
    expect(screen.getAllByText("iOS is not offered on this cluster yet")).toHaveLength(1);
  });
});
