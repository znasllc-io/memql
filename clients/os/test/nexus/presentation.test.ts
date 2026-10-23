import { describe, expect, it } from "vitest";
import { stepOutcome, stepReason, stepTitle, type PresentableStep } from "../../src/nexus/presentation";
const step: PresentableStep = { key: "61e9a1b52a0fb196", callName: "", kind: "deterministic", stepType: "builtin", status: "done" };
describe("readable activity", () => {
  it("uses authored actions and recorded outcomes without inventing a reason", () => {
    const action = { ...step, displayTitle: "Review the invoices", result: { summary: "18 invoices match their approved hours." } };
    expect(stepTitle(action)).toBe("Review the invoices");
    expect(stepOutcome(action)).toBe("18 invoices match their approved hours.");
    expect(stepReason(action)).toBe("");
    expect(stepReason({ ...action, result: { reason: "The saved billing rate changed." } })).toBe("The saved billing rate changed.");
  });
  it("does not print opaque IDs or turn a completed action into an invented successful outcome", () => {
    expect(stepTitle(step)).toBe("Carry out a task");
    expect(stepOutcome(step)).toBe("Completed. No written summary is available.");
    expect(stepTitle({ ...step, callName: "readInvoiceTotals" })).toBe("Read invoice totals");
  });
});
