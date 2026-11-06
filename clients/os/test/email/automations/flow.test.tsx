import { fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { describe, expect, it } from "vitest";
import { Flow } from "../../../src/apps/email/automations/Flow";
import { newStep, moveStep, wireSteps, countQueries, STEP_ID, stepSentence, type FlowStep } from "../../../src/apps/email/automations/flowModel";
import { journeyIssues } from "../../../src/apps/email/automations/issues";
import { journeyActs } from "../../../src/apps/email/automations/automationActs";
import { journeyFromRow, audienceFromRow, templateFromRow, senderIdentityFromRow } from "../../../src/apps/email/rows";
import { addSteps } from "../../../src/apps/email/automations/addModel";
import { audienceRow, templateRow, senderRow } from "../harness";

const steps: FlowStep[] = [{ id: "one", kind: "send", templateId: "t1" }, { id: "two", kind: "wait", days: 3 }];
describe("Automation step model", () => {
  it("keeps IDs when moving, mints distinct valid IDs, and sends only kind fields", () => {
    expect(moveStep(steps, 1, -1).map(s => s.id)).toEqual(["two", "one"]);
    const first = newStep("confirm"), second = newStep("confirm"); expect(first.id).toMatch(STEP_ID); expect(first.id).not.toBe(second.id);
    expect(wireSteps([{ id: "w", kind: "wait", templateId: "discard", days: 1 }, { id: "s", kind: "send", senderIdentityId: "", hours: 3 }])).toEqual([{ id: "w", kind: "wait", days: 1, hours: 0 }, { id: "s", kind: "send" }]);
  });
  it("explains the steps and counts waiting people by the next step", () => {
    expect(stepSentence({ id: "w", kind: "wait", days: 1, hours: 6 }, { template: "" })).toBe("Wait 1 day and 6 hours");
    const queries = countQueries("j", [{ id: "c", kind: "confirm" }, { id: "w", kind: "wait" }, { id: "s", kind: "send" }]);
    expect(queries.find(q => q.cardId === "w")?.args).toEqual({ journeyId: "j", status: "active", stepId: "s" });
    expect(queries.find(q => q.cardId === "c")?.args).toEqual({ journeyId: "j", status: "waiting", stepId: "c" });
    expect(queries.filter(q => q.cardId === "end")).toHaveLength(3);
  });
  it("makes activation absent while dirty or invalid, and archived has no acts", () => {
    const j = journeyFromRow({ id: "j", status: "paused" });
    expect(journeyActs(j, true, [], "").map(a => a.id)).toEqual(["discard", "save"]);
    expect(journeyActs(j, false, [{ stepId: "", text: "Fix" }], "").map(a => a.id)).toEqual(["retire"]);
    expect(journeyActs({ ...j, status: "archived" }, false, [], "")).toEqual([]);
  });
  it("has the event questions and omits sender for cluster recipients", () => {
    expect(addSteps("event", "cluster_roles", false)).toEqual(["trigger", "what", "event", "condition", "who", "template", "review"]);
    expect(addSteps("event", "audience", true)).toContain("sender");
  });
  it("marks confirmation, draft, wrong-organization and wait issues with the engine's words", () => {
    const feeds = { audiences: [audienceFromRow(audienceRow({ id: "a", accountId: "org", confirmation: "required" }))], templates: [templateFromRow(templateRow({ id: "t", accountId: "org", name: "Welcome", status: "draft" }))], senders: [senderIdentityFromRow(senderRow({ id: "s", accountId: "other" }))], confirms: new Map([["t", false]]) };
    const issues = journeyIssues({ accountId: "org", trigger: { kind: "joined", audienceId: "a" }, steps: [{ id: "s1", kind: "send", templateId: "t", senderIdentityId: "s" }, { id: "w", kind: "wait", days: 0 }] }, feeds);
    expect(issues).toContainEqual({ stepId: "", text: "This audience asks people to confirm, so the first email must ask them to confirm." });
    expect(issues).toContainEqual({ stepId: "s1", text: "Publish “Welcome” before turning this on." });
    expect(issues).toContainEqual({ stepId: "s1", text: "Choose a sender in this organization." });
    expect(issues).toContainEqual({ stepId: "w", text: "A wait lasts from one hour to 365 days." });
    expect(journeyIssues({ accountId: "org", trigger: { kind: "confirmed", audienceId: "a" }, steps: [{ id: "c", kind: "confirm", templateId: "t" }] }, feeds)).toContainEqual({ stepId: "c", text: "Add {{confirmUrl}} to “Welcome” so people can confirm." });
  });
});
describe("Flow keyboard", () => {
  function Fixture({ editable = true }: { editable?: boolean }) { const [held, setHeld] = useState(steps); return <Flow trigger="When someone joins Newsletter" accountId="self" steps={held} templates={[templateFromRow(templateRow({ id: "t1", name: "Welcome" }))]} senders={[]} editable={editable} onChange={setHeld} />; }
  it("reaches cards and insertion points, moves and removes without a pointer", () => {
    render(<Fixture />); const trigger = screen.getByRole("button", { name: "When someone joins Newsletter" }); trigger.focus();
    fireEvent.keyDown(trigger, { key: "ArrowDown" }); expect(document.activeElement).toBe(screen.getByRole("button", { name: "Insert step 1" }));
    fireEvent.keyDown(document.activeElement!, { key: "ArrowDown" }); expect(document.activeElement?.getAttribute("data-node")).toBe("one");
    fireEvent.keyDown(document.activeElement!, { key: "ArrowDown", altKey: true });
    const cards = [...document.querySelectorAll(".email-flow-card")]; expect(cards[0]?.textContent).toContain("Wait 3 days");
    fireEvent.keyDown(document.activeElement!, { key: "Delete" }); expect(screen.queryByRole("button", { name: /Send Welcome/ })).toBeNull();
  });
  it("inserts from the menu and readonly has no insertion or move controls", () => {
    const view = render(<Fixture />); fireEvent.click(screen.getByRole("button", { name: "Insert step 1" })); fireEvent.click(screen.getByRole("button", { name: "Ask to confirm" })); expect(screen.getByLabelText("Wait up to")).toBeTruthy();
    view.rerender(<Fixture editable={false} />); expect(screen.queryByRole("button", { name: /Insert step/ })).toBeNull(); expect(screen.queryByRole("button", { name: "Move down" })).toBeNull();
  });
});
