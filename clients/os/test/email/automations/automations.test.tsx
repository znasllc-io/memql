import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { readTheBar } from "../../readTheBar";
import { chooseOption } from "../../selectControl";
import { audienceRow, fakeConnection, recipientRow, rowsResult, ruleRow, senderRow, templateRow, withSession, type FakeSeed } from "../harness";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../../src/live/connection", () => ({ useOsConnection: () => h.connection, bridgePathFor: (base: string) => base + "_memql/ws", osBridgePath: "/_memql/ws" }));
vi.mock("../../../src/auth/context", () => ({ useAuthSource: () => ({ bearer: async () => "token", refresh: async () => {} }) }));
const { EmailApp } = await import("../../../src/apps/email/EmailApp");
const { LocalEmailSettingsStore } = await import("../../../src/apps/email/settings");
const journey = { id: "j", accountId: "self", name: "Welcome series", description: "", status: "draft", trigger: { kind: "joined", audienceId: "a1" }, steps: [{ id: "send1", kind: "send", templateId: "t1", senderIdentityId: "s1" }], createdAt: "2026-10-11T00:00:00Z" };
function setup(seed: FakeSeed = {}) {
  return fakeConnection({ audiences: [audienceRow({ id: "a1", accountId: "self" })], templates: [templateRow({ id: "t1", accountId: "self", name: "Welcome" })], senderIdentities: [senderRow({ id: "s1", accountId: "self" })], ...seed });
}
function mount(conn: ReturnType<typeof fakeConnection>, intent?: { id: string; payload: Record<string, string> }) {
  h.connection = conn; const store = new LocalEmailSettingsStore({ getItem: () => null, setItem: () => {} });
  return render(withSession(<EmailApp sectionId="automations" navigate={() => {}} askContext={() => {}} uploads={{ upload: vi.fn() }} store={store} intent={intent} consumeIntent={() => {}} />));
}
async function press(name: string) { await readTheBar(); fireEvent.click(screen.getByRole("button", { name })); }
describe("Automation pages", () => {
  it("merges journeys and event rules, keeps paused and filters by trigger", async () => {
    mount(setup({ journeys: [journey, { ...journey, id: "retired", name: "Old", status: "archived" }, { ...journey, id: "paused", name: "Paused series", status: "paused" }], emailRules: [ruleRow({ id: "r", name: "New admin" })] }));
    await screen.findByText("Welcome series"); expect(screen.getByText("Paused series")).toBeTruthy(); expect(screen.getByText("New admin")).toBeTruthy(); expect(screen.queryByText("Old")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Refine automations" })); chooseOption(screen.getByLabelText("Starts when"), "Someone joins"); expect(screen.queryByText("New admin")).toBeNull();
  });
  it("opens a journey intent, holds unsaved edits and the original revision after a refusal", async () => {
    const conn = setup({ journeys: [journey] }); conn.query.campaignSaveJourney.mockRejectedValue(new Error("This automation changed; reload before saving."));
    mount(conn, { id: "open-j", payload: { journeyId: "j" } });
    fireEvent.change(await screen.findByLabelText("Name"), { target: { value: "Edited welcome" } });
    expect(screen.queryByRole("button", { name: "Turn on" })).toBeNull(); await press("Save");
    await screen.findByText("This automation changed; reload before saving."); expect((screen.getByLabelText("Name") as HTMLInputElement).value).toBe("Edited welcome");
    expect(conn.query.campaignSaveJourney).toHaveBeenCalledWith(expect.objectContaining({ expectedRevision: journey.createdAt, name: "Edited welcome" }));
  });
  it("turns on after asking, pauses, retires and reads counts", async () => {
    const conn = setup({ journeys: [journey] }); let row = { ...journey };
    conn.query.campaignSetJourneyState.mockImplementation(async args => { row = { ...row, status: args.action === "activate" ? "active" : args.action === "pause" ? "paused" : "archived", createdAt: new Date(Date.parse(row.createdAt) + 1000).toISOString() }; conn.query.journeys.mockResolvedValue(rowsResult([row])); return rowsResult([{ revision: row.createdAt, issues: [] }]); });
    mount(conn); fireEvent.click(await screen.findByText("Welcome series")); await screen.findByRole("button", { name: "Turn on" }); await press("Turn on");
    expect(conn.query.campaignSetJourneyState).not.toHaveBeenCalled(); await press("Turn on"); await screen.findByRole("button", { name: "Pause" }); await press("Pause"); await screen.findByRole("button", { name: "Turn on" }); await press("Retire"); await press("Retire");
    await waitFor(() => expect(conn.query.campaignSetJourneyState.mock.calls.map(c => c[0].action)).toEqual(["activate", "pause", "archive"]));
    expect(conn.query.journeyEnrollmentCounts).toHaveBeenCalledWith({ journeyId: "j" }, expect.anything());
    expect(document.querySelector("button:disabled")).toBeNull();
  });
  it("automation page shows people by status and retries failed ones", async () => {
    const active = { ...journey, status: "active", activatedAt: "2026-10-11T01:00:00Z" };
    const enrollmentCounts: Record<string, number> = { ":active": 3, ":completed": 7, ":failed": 2 };
    const failedRun = (id: string, recipientId: string, lastError: string) => ({ id, journeyId: "j", recipientId, stepId: "send1", stepIndex: 0, status: "failed", lastError, createdAt: "2026-10-11T02:00:00Z" });
    const conn = setup({ journeys: [active], enrollmentCounts, recipientsForAudience: [recipientRow({ id: "r1", email: "one@acme.test" }), recipientRow({ id: "r2", email: "two@acme.test" })],
      enrollments: [failedRun("e1", "r1", "The sender is turned off."), failedRun("e2", "r2", "The template is a draft.")] });
    conn.query.campaignRetryEnrollments.mockImplementation(async args => { if ((args.enrollmentIds as string[]).length === 0 || (args.enrollmentIds as string[])[0] === "e1") enrollmentCounts[":failed"] = 1; return rowsResult([{ retried: 1 }]); });
    mount(conn); fireEvent.click(await screen.findByText("Welcome series"));
    const people = await screen.findByRole("region", { name: "People" });
    await within(people).findByText("one@acme.test");
    for (const [label, count] of [["In a run", "3"], ["Waiting", "0"], ["Finished", "7"], ["Left early", "0"], ["Failed", "2"]] as const) expect(within(people).getByText(label).parentElement?.textContent).toContain(count);
    expect(within(people).getByText("The sender is turned off.")).toBeTruthy(); expect(within(people).getByText("The template is a draft.")).toBeTruthy();
    expect(within(people).getAllByText("Send Welcome").length).toBe(2);
    enrollmentCounts[":failed"] = 1; conn.query.journeyEnrollmentsByStatus.mockResolvedValue(rowsResult([failedRun("e2", "r2", "The template is a draft.")]));
    fireEvent.click(within(people).getByRole("button", { name: "Retry one@acme.test" }));
    await waitFor(() => expect(conn.query.campaignRetryEnrollments).toHaveBeenCalledWith({ journeyId: "j", enrollmentIds: ["e1"] }));
    await waitFor(() => expect(within(people).queryByText("one@acme.test")).toBeNull());
    expect(within(people).getByText("Failed").parentElement?.textContent).toContain("1");
    conn.query.campaignRetryEnrollments.mockRejectedValue(new Error("This organization is not active, so these emails are held."));
    fireEvent.click(within(people).getByRole("button", { name: "Retry all" }));
    await waitFor(() => expect(conn.query.campaignRetryEnrollments).toHaveBeenLastCalledWith({ journeyId: "j", enrollmentIds: [] }));
    await screen.findByText("This organization is not active, so these emails are held.");
  });
  it("leaves Turn on absent and says why in the bar while the automation has something to fix", async () => {
    mount(setup({ journeys: [{ ...journey, steps: [{ id: "send1", kind: "send", templateId: "t1", senderIdentityId: "" }] }], templates: [templateRow({ id: "t1", accountId: "self", name: "Welcome", status: "draft" })] }));
    fireEvent.click(await screen.findByText("Welcome series"));
    const bar = await screen.findByRole("group", { name: "What you can do with this" });
    await waitFor(() => expect(bar.textContent).toMatch(/Can't turn on yet: Publish “Welcome” before turning this on\./));
    expect(screen.queryByRole("button", { name: "Turn on" })).toBeNull();
    expect(document.querySelector("button:disabled")).toBeNull();
  });
  it("shows an event-rule refusal once and keeps code and failure context without internal facts", async () => {
    const sentence = "arming this rule is refused"; const row = ruleRow({ id: "r", name: "New admin", status: "failed", condition: 'row.role == "admin"', lastError: `emailrules: ${sentence}` });
    const conn = setup({ emailRules: [row] }); conn.query.campaignActivateEmailRule.mockRejectedValue(new Error(`emailrules: ${sentence}`)); mount(conn);
    fireEvent.click(await screen.findByText("New admin")); expect(screen.getByText(`Only when ${row.condition}`).tagName).toBe("CODE"); await press("Turn on"); await press("Turn on");
    await waitFor(() => expect(conn.query.campaignActivateEmailRule).toHaveBeenCalledOnce()); expect((document.body.textContent ?? "").split(sentence)).toHaveLength(2); expect(screen.queryByText("Runs as")).toBeNull(); expect(screen.queryByText("Bundle")).toBeNull();
  });
  it.each([undefined, [], [{}], [{ authoredAutomationsEnabled: true }], new Error("denied")])("never treats unknown cluster switch as off", async clusterSettings => {
    mount(setup({ clusterSettings })); await screen.findByText("No automations yet"); expect(screen.queryByText(/Event emails are switched off/)).toBeNull();
  });
  it("names the explicit cluster switch once", async () => { mount(setup({ clusterSettings: [{ authoredAutomationsEnabled: false }] })); await screen.findByText(/Event emails are switched off across this cluster/); });
});
describe("Adding and describing", () => {
  it("creates once under a stable id, then turns on with the saved revision", async () => {
    const conn = setup(); mount(conn); fireEvent.click(await screen.findByRole("button", { name: "Add an automation" }));
    expect(screen.getByRole("button", { name: "Describe it instead" })).toBeTruthy();
    fireEvent.click(screen.getByRole("radio", { name: "Someone joins an audience" })); fireEvent.click(await screen.findByRole("radio", { name: "Newsletter" }));
    fireEvent.click(screen.getByRole("button", { name: /Send an email: choose a template/ })); chooseOption(screen.getByLabelText("Template"), "Welcome"); await press("Next");
    await press("Turn on"); fireEvent.click(screen.getByRole("button", { name: "Turn on" })); expect(conn.query.campaignSaveJourney).not.toHaveBeenCalled(); await press("Turn on");
    await waitFor(() => expect(conn.query.campaignSetJourneyState).toHaveBeenCalledWith(expect.objectContaining({ expectedRevision: "2026-10-12T00:00:00Z", action: "activate" })));
    expect(conn.query.campaignSaveJourney).toHaveBeenCalledOnce(); expect(conn.query.campaignSetJourneyState.mock.calls[0]?.[0].journeyId).toBe(conn.query.campaignSaveJourney.mock.calls[0]?.[0].journeyId);
  });
  it("keeps one event-rule draft after activation refusal and saves later edits before retrying", async () => {
    const conn = setup({ concepts: [{ id: "v1:identity:user", domain: "identity", entity: "user" }] });
    conn.query.campaignActivateEmailRule.mockRejectedValueOnce(new Error("Publish the selected template before turning this on."));
    mount(conn); fireEvent.click(await screen.findByRole("button", { name: "Add an automation" }));
    fireEvent.click(screen.getByRole("radio", { name: "Something happens in MemQL" }));
    fireEvent.click(await screen.findByRole("radio", { name: "user (identity)" }));
    await press("Next"); await press("Next"); await press("Next"); await press("Next");
    chooseOption(screen.getByLabelText("Template"), "Welcome"); await press("Next");
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "A new person" } });
    await press("Turn on"); await press("Turn on");
    await screen.findByText("Publish the selected template before turning this on.");
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "New people" } });
    await press("Turn on"); await press("Turn on");
    await waitFor(() => expect(conn.query.campaignActivateEmailRule).toHaveBeenCalledTimes(2));
    expect(conn.query.createEmailRule).toHaveBeenCalledOnce();
    expect(conn.query.updateEmailRule).toHaveBeenCalledWith(expect.objectContaining({ name: "New people" }));
    expect(conn.query.campaignActivateEmailRule.mock.calls[1]?.[0]).toEqual(conn.query.campaignActivateEmailRule.mock.calls[0]?.[0]);
  });
  it("starts a confirming audience with a Confirm card, and missing confirmation tag blocks activation", async () => {
    const conn = setup({ audiences: [audienceRow({ id: "a1", accountId: "self", confirmation: "required" })] }); mount(conn);
    fireEvent.click(await screen.findByRole("button", { name: "Add an automation" })); fireEvent.click(screen.getByRole("radio", { name: "Someone joins an audience" })); fireEvent.click(await screen.findByRole("radio", { name: /Newsletter/ }));
    fireEvent.click(screen.getByRole("button", { name: /Ask to confirm: choose a template/ })); chooseOption(screen.getByLabelText("Template"), "Welcome"); await screen.findByText("Add {{confirmUrl}} to “Welcome” so people can confirm."); await press("Next"); expect(screen.queryByRole("button", { name: "Turn on" })).toBeNull();
  });
  it("describes without writing, retains a refusal, renders a proposal and edits it as steps", async () => {
    const conn = setup(); conn.query.campaignDescribeJourney.mockRejectedValueOnce(new Error("Couldn't draft this automation: try a clearer audience.")); mount(conn);
    fireEvent.click(await screen.findByRole("button", { name: "Add an automation" })); fireEvent.click(screen.getByRole("button", { name: "Describe it instead" }));
    fireEvent.change(screen.getByLabelText("Describe the automation"), { target: { value: "Welcome newsletter subscribers" } }); await press("Draft it"); await screen.findByText("Couldn't draft this automation: try a clearer audience.");
    expect((screen.getByLabelText("Describe the automation") as HTMLTextAreaElement).value).toBe("Welcome newsletter subscribers"); await press("Draft it"); await screen.findByText("Welcome new subscribers.");
    expect(document.querySelector('[data-proposal="true"]')).toBeTruthy(); expect(conn.query.campaignSaveJourney).not.toHaveBeenCalled(); expect(screen.queryByRole("button", { name: "Check it" })).toBeNull();
    await press("Edit as steps"); expect(screen.getByRole("button", { name: /Send Welcome/ })).toBeTruthy();
  });
  it("discards a proposal when the words change", async () => {
    mount(setup()); fireEvent.click(await screen.findByRole("button", { name: "Add an automation" })); fireEvent.click(screen.getByRole("button", { name: "Describe it instead" })); const text = screen.getByLabelText("Describe the automation"); fireEvent.change(text, { target: { value: "Welcome people" } }); await press("Draft it"); await screen.findByText("Welcome new subscribers."); fireEvent.change(text, { target: { value: "A different plan" } }); expect(document.querySelector('[data-proposal="true"]')).toBeNull(); expect(screen.getByRole("button", { name: "Draft it" })).toBeTruthy();
  });
  it.each(["proposal", "refusal"] as const)("ignores a late %s for words that changed while drafting", async outcome => {
    const conn = setup();
    let resolve!: (value: ReturnType<typeof rowsResult>) => void;
    let reject!: (error: Error) => void;
    conn.query.campaignDescribeJourney.mockImplementationOnce(() => new Promise((yes, no) => { resolve = yes; reject = no; }));
    mount(conn);
    fireEvent.click(await screen.findByRole("button", { name: "Add an automation" }));
    fireEvent.click(screen.getByRole("button", { name: "Describe it instead" }));
    const text = screen.getByLabelText("Describe the automation");
    fireEvent.change(text, { target: { value: "An old request" } });
    await press("Draft it");
    await waitFor(() => expect(conn.query.campaignDescribeJourney).toHaveBeenCalledOnce());
    fireEvent.change(text, { target: { value: "A different plan" } });
    await act(async () => {
      if (outcome === "refusal") reject(new Error("The old request was refused."));
      else resolve(rowsResult([{ proposal: { name: "Old proposal", trigger: journey.trigger, steps: journey.steps }, explanation: "The old request was drafted.", issues: [] }]));
    });
    expect((text as HTMLTextAreaElement).value).toBe("A different plan");
    expect(document.querySelector('[data-proposal="true"]')).toBeNull();
    expect(screen.queryByText(/The old request was/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Turn on" })).toBeNull();
    await press("Draft it");
    await screen.findByText("Welcome new subscribers.");
    expect(conn.query.campaignDescribeJourney).toHaveBeenLastCalledWith({ accountId: "self", sentence: "A different plan" });
    expect(conn.query.campaignSaveJourney).not.toHaveBeenCalled();
  });
  it("leaves a new description alone when the previous page unmounted during drafting", async () => {
    const conn = setup();
    let resolve!: (value: ReturnType<typeof rowsResult>) => void;
    conn.query.campaignDescribeJourney.mockImplementationOnce(() => new Promise(yes => { resolve = yes; }));
    const view = mount(conn);
    fireEvent.click(await screen.findByRole("button", { name: "Add an automation" }));
    fireEvent.click(screen.getByRole("button", { name: "Describe it instead" }));
    fireEvent.change(screen.getByLabelText("Describe the automation"), { target: { value: "Old page" } });
    await press("Draft it");
    view.unmount();
    mount(conn);
    fireEvent.click(await screen.findByRole("button", { name: "Add an automation" }));
    fireEvent.click(screen.getByRole("button", { name: "Describe it instead" }));
    fireEvent.change(screen.getByLabelText("Describe the automation"), { target: { value: "New page" } });
    await act(async () => { resolve(rowsResult([{ proposal: { name: "Old proposal", trigger: journey.trigger, steps: journey.steps }, explanation: "The old page was drafted.", issues: [] }])); });
    expect(screen.queryByText("The old page was drafted.")).toBeNull();
    expect(document.querySelector('[data-proposal="true"]')).toBeNull();
    expect((screen.getByLabelText("Describe the automation") as HTMLTextAreaElement).value).toBe("New page");
    expect(screen.getByRole("button", { name: "Draft it" })).toBeTruthy();
    expect(conn.query.campaignSaveJourney).not.toHaveBeenCalled();
  });
});
