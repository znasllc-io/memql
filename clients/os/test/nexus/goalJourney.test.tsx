import { useState } from "react";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { GoalJourney } from "../../src/apps/nexus/GoalJourney";
import { StepSummary } from "../../src/apps/nexus/StepSummary";
import { stepFromRow, type StepRow } from "../../src/apps/nexus/rows";
import { stepRow } from "./harness";

function Journey({ steps }: { steps: StepRow[] }) {
  const [selected, select] = useState("");
  const active = steps.find(step => step.key === selected);
  return <><GoalJourney steps={steps} selected={selected} onSelect={select} completed={steps.filter(s => s.status === "done").length} total={steps.length} />{active ? <StepSummary step={active} /> : null}</>;
}
const row = (i: number, fields: object = {}) => stepFromRow(stepRow({ id: `s-${i}`, key: `step-${i}`, seq: i, displayTitle: `Review client ${i + 1}`, result: { summary: "Billing details checked." }, ...fields }));

describe("the readable goal journey", () => {
  it("keeps the recent and current work visible in a 55-step history, and can expand and collapse a selected earlier action", () => {
    render(<Journey steps={Array.from({ length: 55 }, (_, i) => row(i, { status: i < 54 ? "done" : "running" }))} />);
    const journey = screen.getByRole("region", { name: "Goal journey" });
    expect(within(journey).getAllByRole("listitem")).toHaveLength(4);
    expect(screen.getByText("54 of 55 steps complete")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Show 51 earlier completed steps" }));
    expect(within(journey).getAllByRole("listitem")).toHaveLength(55);
    const action = screen.getByRole("button", { name: "Review client 1, Done" });
    action.focus(); fireEvent.click(action);
    expect(document.activeElement).toBe(action);
    expect(action.getAttribute("aria-expanded")).toBe("true");
    fireEvent.click(screen.getByRole("button", { name: "Collapse earlier completed steps" }));
    expect(within(journey).getAllByRole("listitem")).toHaveLength(4);
    expect(screen.queryByLabelText("Selected action")).toBeNull();
  });

  it("uses native focusable buttons and one selection while exposing real reasons, unknown usage and technical evidence separately", () => {
    render(<Journey steps={[row(0, { result: { summary: "The folder refused the invoice file.", reason: "The output folder is read-only." }, status: "failed", errorMessage: "EACCES internal-ref-abc" }), row(1, { status: "waiting", result: null })]} />);
    const failed = screen.getByRole("button", { name: "Review client 1, Failed" });
    expect(failed.tagName).toBe("BUTTON"); expect(failed.tabIndex).toBe(0);
    failed.focus(); fireEvent.click(failed);
    const selected = screen.getByLabelText("Selected action");
    expect(within(selected).getByText("The output folder is read-only.")).toBeTruthy();
    expect(within(selected).getByText("Not reported")).toBeTruthy();
    const technical = within(selected).getByText("Technical details").closest("details")!;
    expect(technical.open).toBe(false);
    expect(within(technical).getByText("EACCES internal-ref-abc")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Review client 2, Waiting" }));
    expect(failed.getAttribute("aria-expanded")).toBe("false");
    expect(within(screen.getByLabelText("Selected action")).queryByText("Why")).toBeNull();
  });
});
