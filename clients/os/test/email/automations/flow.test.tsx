import { fireEvent, render, screen, within } from "@testing-library/react";
import { useState } from "react";
import { describe, expect, it } from "vitest";
import { Flow } from "../../../src/apps/email/automations/Flow";
import { newStep, moveStep, wireSteps, countQueries, STEP_ID, stepSentence, type FlowStep } from "../../../src/apps/email/automations/flowModel";
import { journeyIssues } from "../../../src/apps/email/automations/issues";
import { journeyActs } from "../../../src/apps/email/automations/automationActs";
import { journeyFromRow, audienceFromRow, templateFromRow, senderIdentityFromRow } from "../../../src/apps/email/rows";
import { addSteps } from "../../../src/apps/email/automations/addModel";
import { RUN_STATUSES } from "../../../src/apps/email/useEmail";
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
    expect(stepSentence({ id: "w", kind: "wait", minutes: 5 }, { template: "" })).toBe("Wait 5 minutes");
    expect(stepSentence({ id: "w", kind: "wait", minutes: 1 }, { template: "" })).toBe("Wait 1 minute");
    expect(stepSentence({ id: "w", kind: "wait", days: 1, hours: 2, minutes: 3 }, { template: "" })).toBe("Wait 1 day, 2 hours and 3 minutes");
    // Minutes go on the wire only when there are some, so a days-long wait is sent exactly as before.
    expect(wireSteps([{ id: "w", kind: "wait", days: 0, hours: 0, minutes: 5 }])).toEqual([{ id: "w", kind: "wait", days: 0, hours: 0, minutes: 5 }]);
    const queries = countQueries("j", [{ id: "c", kind: "confirm" }, { id: "w", kind: "wait" }, { id: "s", kind: "send" }]);
    expect(queries.find(q => q.cardId === "w")?.args).toEqual({ journeyId: "j", status: "active", stepId: "s" });
    expect(queries.find(q => q.cardId === "c")?.args).toEqual({ journeyId: "j", status: "waiting", stepId: "c" });
    expect(queries.filter(q => q.cardId === "end")).toHaveLength(3);
  });
  it("names each run state with the words the People section uses", () => {
    // `waiting` is a run waiting for the person to confirm (the engine writes it
    // only at a confirm step); a wait step's runs are running, sitting in the wait.
    const label = (cardId: string, status?: string) => countQueries("j", [{ id: "c", kind: "confirm" }, { id: "w", kind: "wait" }, { id: "s", kind: "send" }]).find(q => q.cardId === cardId && q.args.status === status)?.label;
    expect(label("c", "waiting")).toBe(RUN_STATUSES.find(([status]) => status === "waiting")?.[1].toLowerCase());
    expect(label("c", "waiting")).toBe("waiting to confirm");
    expect(label("end", "completed")).toBe(RUN_STATUSES.find(([status]) => status === "completed")?.[1].toLowerCase());
    expect(label("end", "completed")).toBe("finished");
    expect(label("end", "exited")).toBe(RUN_STATUSES.find(([status]) => status === "exited")?.[1].toLowerCase());
    expect(label("w", "active")).toBe("waiting");
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
    expect(issues).toContainEqual({ stepId: "w", text: "A wait lasts from one minute to 365 days." });
    const wait = (step: Partial<FlowStep>) => journeyIssues({ accountId: "org", trigger: { kind: "joined", audienceId: "a" }, steps: [{ id: "w", kind: "wait", ...step }] }, feeds).filter(i => i.stepId === "w");
    expect(wait({ minutes: 5 })).toEqual([]);
    expect(wait({ minutes: 60 })).toEqual([{ stepId: "w", text: "A wait lasts from one minute to 365 days." }]);
    expect(wait({ days: 365, minutes: 1 })).toEqual([{ stepId: "w", text: "A wait lasts from one minute to 365 days." }]);
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
  // The Days / Minutes switch (owner's direction, 2026-11-10): testers turn a
  // "5 days" wait into "5 minutes" to walk an automation end to end. Switching
  // keeps the length of time; switching back rounds to whole days, at least 1.
  it("switches a wait between days and minutes, converting the number", () => {
    render(<Fixture />);
    fireEvent.click(screen.getByRole("button", { name: "Wait 3 days" }));
    const unit = screen.getByRole("radiogroup", { name: "Wait unit" });
    const length = () => (screen.getByLabelText("Wait") as HTMLInputElement).value;
    expect(within(unit).getByRole("radio", { name: "Days" }).getAttribute("aria-checked")).toBe("true");
    expect(length()).toBe("3");
    fireEvent.click(within(unit).getByRole("radio", { name: "Minutes" }));
    expect(length()).toBe("4320");
    expect(screen.getByRole("button", { name: "Wait 3 days" })).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Wait"), { target: { value: "5" } });
    expect(screen.getByRole("button", { name: "Wait 5 minutes" })).toBeTruthy();
    fireEvent.click(within(unit).getByRole("radio", { name: "Days" }));
    expect(length()).toBe("1");
    expect(screen.getByRole("button", { name: "Wait 1 day" })).toBeTruthy();
  });
  it("shows the step afresh when its length changes from outside, as Discard changes does", () => {
    const original: FlowStep[] = [{ id: "w", kind: "wait", days: 3 }];
    function Held({ steps: given }: { steps: FlowStep[] }) { const [held, setHeld] = useState(given); const [seen, setSeen] = useState(given); if (seen !== given) { setSeen(given); setHeld(given); } return <Flow trigger="When someone joins Newsletter" accountId="self" steps={held} templates={[]} senders={[]} editable onChange={setHeld} />; }
    const view = render(<Held steps={original} />);
    fireEvent.click(screen.getByRole("button", { name: "Wait 3 days" }));
    fireEvent.click(within(screen.getByRole("radiogroup", { name: "Wait unit" })).getByRole("radio", { name: "Minutes" }));
    fireEvent.change(screen.getByLabelText("Wait"), { target: { value: "5" } });
    expect(screen.getByRole("button", { name: "Wait 5 minutes" })).toBeTruthy();
    view.rerender(<Held steps={[{ id: "w", kind: "wait", days: 3 }]} />);
    expect(screen.getByRole("button", { name: "Wait 3 days" })).toBeTruthy();
    expect((screen.getByLabelText("Wait") as HTMLInputElement).value).toBe("3");
    expect(within(screen.getByRole("radiogroup", { name: "Wait unit" })).getByRole("radio", { name: "Days" }).getAttribute("aria-checked")).toBe("true");
  });
  it("opens a wait that is not whole days in minutes", () => {
    function Odd() { const [held, setHeld] = useState<FlowStep[]>([{ id: "w", kind: "wait", days: 1, hours: 6 }]); return <Flow trigger="When someone joins Newsletter" accountId="self" steps={held} templates={[]} senders={[]} editable onChange={setHeld} />; }
    render(<Odd />);
    fireEvent.click(screen.getByRole("button", { name: "Wait 1 day and 6 hours" }));
    expect(within(screen.getByRole("radiogroup", { name: "Wait unit" })).getByRole("radio", { name: "Minutes" }).getAttribute("aria-checked")).toBe("true");
    expect((screen.getByLabelText("Wait") as HTMLInputElement).value).toBe("1800");
  });
  it("inserts from the menu and readonly has no insertion or move controls", () => {
    const view = render(<Fixture />); fireEvent.click(screen.getByRole("button", { name: "Insert step 1" })); fireEvent.click(screen.getByRole("button", { name: "Ask to confirm" })); expect(screen.getByLabelText("Wait up to")).toBeTruthy();
    view.rerender(<Fixture editable={false} />); expect(screen.queryByRole("button", { name: /Insert step/ })).toBeNull(); expect(screen.queryByRole("button", { name: "Move down" })).toBeNull();
  });
});
