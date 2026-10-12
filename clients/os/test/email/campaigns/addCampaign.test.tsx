import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import { readTheBar } from "../../readTheBar";
import { chooseOption } from "../../selectControl";
import { installSeededAccess } from "../../seededAccess";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

vi.mock("../../../src/auth/context", () => ({
  useAuthSource: () => ({ bearer: async () => "test-token", refresh: async () => {} }),
}));

const { EmailApp } = await import("../../../src/apps/email/EmailApp");
const { LocalEmailSettingsStore } = await import("../../../src/apps/email/settings");
const { saveEditorPreference } = await import("../../../src/items/editorPreference");
const { VSCODE_NO_ANSWER_MESSAGE, browserHandoffPorts } = await import("../../../src/items/vscode");
const { audienceRow, campaignRow, fakeConnection, rowsResult, senderRow, templateRow, withSession } = await import("../harness");

// ADD A CAMPAIGN, ON THE SHELL'S ONE WIZARD (memql#6033, Ruling 8).
//
// One question a step, and a step that is one choice is answered by choosing.
// Nothing is written until the person acts on Review, and then only under the
// wizard's own id: a double press is one campaign and one send. Pinned in one
// zone with no daylight saving, so a chosen local time is the same instant on
// every machine that runs the suite.

type Conn = ReturnType<typeof fakeConnection>;

const CAMPAIGN = "v1:campaigns:campaign";
const AUDIENCE = "v1:campaigns:audience";
const TEMPLATE = "v1:campaigns:template";
const ZONE = "America/Phoenix";
let previousZone: string | undefined;

beforeAll(() => {
  previousZone = process.env.TZ;
  process.env.TZ = ZONE;
});

afterAll(() => {
  if (previousZone === undefined) delete process.env.TZ;
  else process.env.TZ = previousZone;
});

afterEach(() => {
  installSeededAccess("owner");
  localStorage.clear();
  vi.useRealTimers();
});

const AUDIENCES = [
  audienceRow({ id: "a1", name: "Newsletter" }),
  audienceRow({ id: "a-test", name: "Reviewers" }),
  audienceRow({ id: "a-old", name: "Old list", status: "archived" }),
];
const TEMPLATES = [
  templateRow({ id: "t1", name: "August copy" }),
  templateRow({ id: "t-draft", name: "Autumn draft", status: "draft" }),
  templateRow({ id: "t-old", name: "Old copy", status: "archived" }),
];
const SENDERS = [
  senderRow({ id: "s1", address: "news@acme.com", fromName: "Acme News" }),
  senderRow({ id: "s-old", address: "old@acme.com", fromName: "Old News", status: "disabled" }),
];
const COUNTS = { a1: 1204, "a-test": 3 };

function seed(over: Parameters<typeof fakeConnection>[0] = {}) {
  return fakeConnection({ audiences: AUDIENCES, templates: TEMPLATES, senderIdentities: SENDERS, subscriberCounts: COUNTS, ...over });
}

/**
 * One Email window, as the shell holds it: it remembers the section the app was
 * last sent to and renders the app on it, so a page that leaves for another tab
 * really leaves, and `goto` is the person pressing a tab to come back.
 */
function mountWindow(connection: Conn, session: { role?: string; accountIds?: string[]; everyAccount?: boolean } = {}) {
  h.connection = connection;
  const navigate = vi.fn();
  const bag = new Map<string, string>();
  const store = new LocalEmailSettingsStore({ getItem: (k) => bag.get(k) ?? null, setItem: (k, v) => void bag.set(k, v) });
  let jump: (section: string) => void = () => {};
  function Window() {
    const [current, setCurrent] = useState("campaigns");
    jump = setCurrent;
    return withSession(
      <EmailApp
        sectionId={current}
        navigate={(id, options) => {
          navigate(id, options);
          setCurrent(id);
        }}
        askContext={() => {}}
        store={store}
        uploads={{ upload: vi.fn() }}
      />,
      session,
    );
  }
  const view = render(<Window />);
  return { view, navigate, goto: (section: string) => act(() => jump(section)) };
}

// ---------------------------------------------------------------------------
// Reading the wizard
// ---------------------------------------------------------------------------

const rail = () => screen.getByRole("list", { name: "Adding a campaign" });
const railLabels = () => within(rail()).getAllByText((_, node) => node?.classList.contains("os-rail-label") ?? false).map((node) => node.textContent);
/** The step whose body is drawn. */
const openStep = () => document.querySelector('.os-rail-stage[data-open="true"] .os-rail-label')?.textContent ?? "";
/** A step's line on the rail: its name, then its answer. */
const railLine = (name: string) => within(rail()).getByRole("button", { name: new RegExp(`^${name}`) });
const choose = (name: RegExp) => fireEvent.click(screen.getByRole("radio", { name }));
const optionNames = () => screen.getAllByRole("radio").map((radio) => radio.querySelector(".os-choice-card-name")?.textContent ?? "");

const bar = () => screen.getByRole("group", { name: "What you can do with this" });
const barWord = () => bar().querySelector(".os-actbar-word")?.textContent ?? "";
const barDetail = () => bar().querySelector(".os-actbar-detail")?.textContent ?? "";
const barActs = () => within(bar()).queryAllByRole("button").map((button) => button.textContent);
const press = (name: string) => fireEvent.click(within(bar()).getByRole("button", { name }));
/** The bar's acts that can be pressed now: not disabled. */
const pressable = () => within(bar()).queryAllByRole("button").filter((button) => !(button as HTMLButtonElement).disabled).map((button) => button.textContent);
const SERIES = "v1:campaigns:campaignSeries";

/** The buttons a person could press that do nothing: disabled, and not because they are working. */
function deadButtons(): string[] {
  return screen
    .queryAllByRole("button")
    .filter((button) => (button as HTMLButtonElement).disabled && button.getAttribute("aria-busy") !== "true")
    .map((button) => button.textContent ?? "");
}

async function openWizard() {
  fireEvent.click(await screen.findByRole("button", { name: "Add a campaign" }));
  expect(await screen.findByRole("heading", { level: 3, name: "Add a campaign" })).toBeTruthy();
}

/** Audience, template and sender answered; When open. */
async function walkToWhen(template: RegExp = /^August copy/) {
  await openWizard();
  // Wait for the count, so every sentence after this can name it.
  await screen.findByRole("radio", { name: /^Newsletter.*1,204 subscribers/ });
  choose(/^Newsletter/);
  choose(template);
  choose(/^news@acme\.com/);
  expect(openStep()).toBe("When");
}

async function walkToReview(template?: RegExp) {
  await walkToWhen(template);
  choose(/^Send now/);
  expect(openStep()).toBe("Review");
  await waitFor(() => expect(barWord()).not.toBe("Review the campaign"));
  // Review is a step the bar has just moved to, and a bar that has just changed takes no press for a moment.
  await readTheBar();
}

const createdCampaign = (conn: Conn) => conn.query.createCampaign.mock.calls[0]![0] as Record<string, unknown>;

// ---------------------------------------------------------------------------
// The questions
// ---------------------------------------------------------------------------

describe("Add a campaign", () => {
  it("asks one question a step and answers a choice by choosing", async () => {
    const conn = seed();
    mountWindow(conn);
    await openWizard();

    // One organization: nothing to ask, so the first question is who gets it.
    expect(railLabels()).toEqual(["Audience", "Template", "Sender", "When", "Review"]);
    expect(openStep()).toBe("Audience");
    expect(barWord()).toBe("Choose an audience");

    // The organization's active audiences, each with how many it reaches.
    expect(await screen.findByRole("radio", { name: /^Newsletter.*1,204 subscribers/ })).toBeTruthy();
    expect(optionNames()).toEqual(["Newsletter", "Reviewers"]);
    // Choosing IS the answer: there is no Next to press.
    expect(barActs()).not.toContain("Next");
    choose(/^Newsletter/);
    expect(openStep()).toBe("Template");
    // The answered step reads back what was said.
    expect(railLine("Audience").textContent).toContain("Newsletter");

    // Published first, drafts after and said to be drafts; archived ones are not offered.
    expect(barWord()).toBe("Choose a template");
    expect(optionNames()).toEqual(["August copy", "Autumn draft"]);
    expect(screen.getByRole("radio", { name: /^Autumn draft/ }).textContent).toContain("Draft");
    choose(/^August copy/);
    expect(openStep()).toBe("Sender");

    // The cluster's own organization may send as the cluster; retired senders are not offered.
    expect(barWord()).toBe("Choose a sender");
    expect(optionNames()).toEqual(["This cluster's sender", "news@acme.com"]);
    expect(screen.getByRole("radio", { name: /^news@acme\.com/ }).textContent).toContain("Acme News");
    choose(/^news@acme\.com/);
    expect(openStep()).toBe("When");
    // An address is read back as it is spelled.
    expect(railLine("Sender").textContent).toContain("news@acme.com");

    expect(optionNames()).toEqual(["Send now", "Schedule", "Repeat"]);
    choose(/^Send now/);
    expect(openStep()).toBe("Review");

    // Review says each answer once: the rail lists them, and one sentence adds
    // only what the rail cannot -- how many people it reaches.
    expect(await screen.findByText("Send “August copy” to Newsletter (1,204 subscribers) as news@acme.com now.")).toBeTruthy();
    expect(document.querySelector(".os-facts")).toBeNull();
    // The name defaults to the template's; the tracking to the preference.
    expect((screen.getByLabelText("Name") as HTMLInputElement).placeholder).toBe("August copy");
    expect((screen.getByRole("switch", { name: "Count who opens it" }) as HTMLInputElement).checked).toBe(true);
    expect((screen.getByRole("switch", { name: "Count who clicks a link" }) as HTMLInputElement).checked).toBe(true);
    await waitFor(() => expect(barWord()).toBe("Ready to send"));
    expect(barActs()).toEqual(["Send a test", "Save as draft", "Send now"]);

    // The cluster's own sender: capitalized where it starts a line, not inside the sentence.
    fireEvent.click(railLine("Sender"));
    choose(/^This cluster's sender/);
    expect(openStep()).toBe("Review");
    expect(railLine("Sender").textContent).toContain("This cluster's sender");
    expect(screen.getByText("Send “August copy” to Newsletter (1,204 subscribers) as this cluster's sender now.")).toBeTruthy();
    // Nothing has been written by asking.
    expect(conn.query.createCampaign).not.toHaveBeenCalled();
  });

  it("asks for the organization only when there is a choice", async () => {
    const conn = seed({
      accounts: [
        { id: "self", name: "Operator organization", status: "active" },
        { id: "acme", name: "Acme", status: "active" },
      ],
      audiences: [...AUDIENCES, audienceRow({ id: "a-acme", name: "Acme list", accountId: "acme" })],
    });
    mountWindow(conn);
    await openWizard();

    // Two to choose between: asked first, and already answered with where this person starts.
    await waitFor(() => expect(railLabels()).toEqual(["Organization", "Audience", "Template", "Sender", "When", "Review"]));
    expect(openStep()).toBe("Audience");
    expect(railLine("Organization").textContent).toContain("Operator organization");
    // Only that organization's audiences.
    expect(await screen.findByRole("radio", { name: /^Newsletter/ })).toBeTruthy();
    expect(screen.queryByRole("radio", { name: /^Acme list/ })).toBeNull();
    choose(/^Newsletter/);

    // Choosing another asks again for everything that belonged to the first.
    fireEvent.click(railLine("Organization"));
    expect(openStep()).toBe("Organization");
    expect(barWord()).toBe("Choose an organization");
    choose(/^Acme$/);
    expect(openStep()).toBe("Audience");
    expect(screen.getByRole("radio", { name: /^Acme list/ })).toBeTruthy();
    expect(screen.queryByRole("radio", { name: /^Newsletter/ })).toBeNull();
    expect(railLine("Audience").textContent).not.toContain("Newsletter");
  });

  it("names the organization in Review's sentence only when there is a choice", async () => {
    const conn = seed({
      accounts: [
        { id: "self", name: "Operator organization", status: "active" },
        { id: "acme", name: "Acme", status: "active" },
      ],
    });
    mountWindow(conn);
    await walkToReview();
    expect(await screen.findByText("Send “August copy” for Operator organization to Newsletter (1,204 subscribers) as news@acme.com now.")).toBeTruthy();
    expect(document.querySelector(".os-facts")).toBeNull();
  });

  it("leaves for Audiences when the organization has none, and is still there on return", async () => {
    const conn = seed({ audiences: [] });
    const win = mountWindow(conn);
    await openWizard();

    expect(await screen.findByText("This organization has no audiences yet.")).toBeTruthy();
    expect(barWord()).toBe("Choose an audience");
    // The step's forward act leaves the wizard, and is still the floor's.
    expect(barActs()).toEqual(["Add an audience"]);
    press("Add an audience");
    expect(win.navigate).toHaveBeenCalledWith("audiences", { fromContent: true });
    // The Audiences tab starts its own Add.
    expect(await screen.findByRole("heading", { level: 3, name: "New audience" })).toBeTruthy();

    // The audience the person adds arrives on the live feed...
    act(() => conn.subscriptions.emit(AUDIENCE, audienceRow({ id: "a-new", name: "Launch list" }), "NODE_CREATED"));
    // ...and coming back finds the same wizard, on the same question, with it to choose.
    win.goto("campaigns");
    expect(await screen.findByRole("radio", { name: /^Launch list/ })).toBeTruthy();
    expect(openStep()).toBe("Audience");
    expect(screen.queryByText("This organization has no audiences yet.")).toBeNull();
    choose(/^Launch list/);
    expect(openStep()).toBe("Template");
  });

  it("creates a new template in the editor and waits for it to be published", async () => {
    const open = vi.spyOn(window, "open").mockImplementation(() => null);
    try {
      const conn = seed({ templates: [templateRow({ id: "t1", name: "August copy" })] });
      mountWindow(conn);
      await openWizard();
      await screen.findByRole("radio", { name: /^Newsletter/ });
      choose(/^Newsletter/);
      expect(openStep()).toBe("Template");

      // The organization has a template, so making another is the quiet act.
      const create = within(bar()).getByRole("button", { name: "Create new template" });
      expect(create.className).toBe("os-actbar-text");
      fireEvent.click(create);

      // The editor opened inside the click, on the New email page for this organization.
      expect(open).toHaveBeenCalledOnce();
      expect(new URL(String(open.mock.calls[0]![0])).searchParams.get("resource")).toBe("memql-file://memql.example.com/new-email/self/New%20email");

      // Somebody else's turn: a visible wait, measured.
      expect(bar().getAttribute("data-tone")).toBe("busy");
      expect(barWord()).toBe("Waiting for the new template");
      expect(barDetail()).toBe("Publish it in the editor to continue.");
      expect(bar().querySelector(".os-actbar-meta")?.textContent).toMatch(/^\d+:\d\d$/);
      expect(barActs()).toEqual(["Cancel", "Stop waiting"]);

      // A template that was already there, published again, is not the new one.
      act(() => conn.subscriptions.emit(TEMPLATE, templateRow({ id: "t1", name: "August copy", createdAt: "2026-10-11T09:00:00Z" })));
      expect(barWord()).toBe("Waiting for the new template");

      // The editor saves a draft first: still waiting, and the bar names it.
      act(() => conn.subscriptions.emit(TEMPLATE, templateRow({ id: "t-new", name: "Welcome 3", status: "draft" }), "NODE_CREATED"));
      expect(barDetail()).toBe("“Welcome 3” is a draft. Publish it in the editor to continue.");
      expect(openStep()).toBe("Template");

      // Published: chosen, and the wizard moves on.
      act(() => conn.subscriptions.emit(TEMPLATE, templateRow({ id: "t-new", name: "Welcome 3", status: "ready" })));
      await waitFor(() => expect(openStep()).toBe("Sender"));
      expect(railLine("Template").textContent).toContain("Welcome 3");
      expect(bar().getAttribute("data-tone")).not.toBe("busy");
    } finally {
      open.mockRestore();
    }
  });

  it("says so in the step when the editor does not answer", async () => {
    saveEditorPreference("vscode");
    const conn = seed();
    mountWindow(conn);
    await openWizard();
    await screen.findByRole("radio", { name: /^Newsletter/ });
    choose(/^Newsletter/);
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    // An installed editor is opened by its own scheme, which a test's browser
    // cannot follow: the page goes nowhere, exactly as when no editor answers.
    const navigate = vi.spyOn(browserHandoffPorts, "navigate").mockImplementation(() => {});
    try {
      press("Create new template");
      expect(navigate.mock.calls[0]![0]).toMatch(/^vscode:\/\/znasllc\.memql-productivity-tools\/open\?resource=memql-file%3A%2F%2Fmemql\.example\.com%2Fnew-email%2Fself%2FNew%2520email/);
      expect(barWord()).toBe("Waiting for the new template");
      act(() => { vi.advanceTimersByTime(3_000); });
    } finally {
      navigate.mockRestore();
      vi.useRealTimers();
    }
    // The wait stops, and the step says what to check.
    expect(screen.getByText(VSCODE_NO_ANSWER_MESSAGE)).toBeTruthy();
    expect(barWord()).toBe("Choose a template");
    expect(bar().getAttribute("data-tone")).not.toBe("busy");
  });

  it("offers Set up sending only to people who may set it up", async () => {
    const acme = () =>
      seed({
        accounts: [{ id: "acme", name: "Acme", status: "active" }],
        audiences: [audienceRow({ id: "a-acme", name: "Acme list", accountId: "acme" })],
        templates: [templateRow({ id: "t-acme", name: "Acme copy", accountId: "acme" })],
        senderIdentities: [],
      });

    // An owner: the step's forward act is the way to set it up.
    const owner = mountWindow(acme(), { accountIds: ["acme"] });
    await openWizard();
    await screen.findByRole("radio", { name: /^Acme list/ });
    choose(/^Acme list/);
    choose(/^Acme copy/);
    expect(openStep()).toBe("Sender");
    // A client organization has no cluster sender to fall back on.
    expect(screen.queryAllByRole("radio")).toEqual([]);
    expect(barActs()).toEqual(["Set up sending"]);
    await readTheBar();
    press("Set up sending");
    expect(owner.navigate).toHaveBeenCalledWith("settings", { fromContent: true });
    expect(await screen.findByRole("heading", { level: 3, name: "Settings" })).toBeTruthy();
    owner.view.unmount();

    // Anybody else: the step says who can, and offers nothing.
    installSeededAccess("writer");
    mountWindow(acme(), { role: "writer", everyAccount: false, accountIds: ["acme"] });
    await openWizard();
    await screen.findByRole("radio", { name: /^Acme list/ });
    choose(/^Acme list/);
    choose(/^Acme copy/);
    expect(openStep()).toBe("Sender");
    expect(screen.getByText("An owner or developer sets up sending for Acme.")).toBeTruthy();
    expect(barActs()).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// Acting on Review
// ---------------------------------------------------------------------------

describe("Add a campaign, on Review", () => {
  it("confirms naming the audience size, then creates once and sends", async () => {
    const conn = seed();
    mountWindow(conn);
    await walkToReview();
    await waitFor(() => expect(barActs()).toEqual(["Send a test", "Save as draft", "Send now"]));

    press("Send now");
    // It asks, in the bar, naming how many people this reaches.
    expect(screen.getByText("Send “August copy” to 1,204 subscribers now?")).toBeTruthy();
    expect(barActs()).toEqual(["Not yet", "Send now"]);
    expect(conn.query.createCampaign).not.toHaveBeenCalled();
    // Not yet writes nothing and asks nothing -- once the question has been read.
    await readTheBar();
    press("Not yet");
    expect(barActs()).toEqual(["Send a test", "Save as draft", "Send now"]);
    expect(screen.queryByText("Send “August copy” to 1,204 subscribers now?")).toBeNull();

    // Not yet put the acts back, and a bar that has just changed takes no press for a moment.
    await readTheBar();
    press("Send now");
    // A double-click asks and does not answer: its second click lands on the
    // confirmation before anybody could have read the question.
    press("Send now");
    expect(conn.query.createCampaign).not.toHaveBeenCalled();
    expect(barActs()).toEqual(["Not yet", "Send now"]);
    await readTheBar();
    const confirm = within(bar()).getByRole("button", { name: "Send now" });
    // Two presses before the first one's busy state is drawn: one campaign, one send.
    act(() => {
      fireEvent.click(confirm);
      fireEvent.click(confirm);
    });
    await waitFor(() => expect(conn.query.campaignStartSend).toHaveBeenCalledTimes(1));
    expect(conn.query.createCampaign).toHaveBeenCalledTimes(1);
    const created = createdCampaign(conn);
    expect(created).toMatchObject({
      name: "August copy",
      audienceId: "a1",
      templateId: "t1",
      accountId: "self",
      senderIdentityId: "s1",
      trackOpens: true,
      trackClicks: true,
    });
    expect(created.scheduledAt).toBeUndefined();
    expect(created).not.toHaveProperty("ownerUserId");
    expect(conn.query.campaignStartSend.mock.calls[0]![0]).toEqual({ campaignId: created.campaignId });

    // Taken, not landed: the floor holds the act it was, busy, and nothing else -- no Not yet.
    await waitFor(() => expect(barWord()).toBe("Starting"));
    expect(barActs()).toEqual(["Send now"]);
    expect(pressable()).toEqual([]);
    // The draft's row arrives first, as it is: the wizard still holds.
    const row = campaignRow({ id: created.campaignId as string, name: "August copy", audienceId: "a1", templateId: "t1", senderIdentityId: "s1" });
    act(() => conn.subscriptions.emit(CAMPAIGN, row, "NODE_CREATED"));
    expect(screen.getByRole("heading", { level: 3, name: "Add a campaign" })).toBeTruthy();
    expect(barWord()).toBe("Starting");
    // The send lands; the campaign's page opens on it, sending -- never on the draft with Send now live.
    act(() => conn.subscriptions.emit(CAMPAIGN, { ...row, status: "sending" }));
    expect(await screen.findByRole("heading", { level: 3, name: "August copy" })).toBeTruthy();
    expect(screen.queryByRole("heading", { level: 3, name: "Add a campaign" })).toBeNull();
    await waitFor(() => expect(barWord()).toBe("Sending"));
    expect(barActs()).not.toContain("Send now");
  });

  it("keeps the draft and says so when the send is refused", async () => {
    const conn = seed();
    conn.query.campaignStartSend.mockRejectedValueOnce(new Error("no email sender is registered on this node"));
    mountWindow(conn);
    await walkToReview();
    await waitFor(() => expect(barActs()).toContain("Send now"));
    await readTheBar();
    press("Send now");
    await readTheBar();
    press("Send now");

    expect(await screen.findByText("The campaign was saved as a draft but not sent.")).toBeTruthy();
    expect(screen.getByText("no email sender is registered on this node")).toBeTruthy();
    // Still on Review, and the floor is the draft's now.
    expect(openStep()).toBe("Review");
    expect(barActs()).toEqual(["Cancel", "Send a test", "Send now"]);

    // Trying again sends the same campaign; it is not written twice. The ask
    // took the refused confirmation's place under its label, so it is read first.
    await readTheBar();
    press("Send now");
    await readTheBar();
    press("Send now");
    await waitFor(() => expect(conn.query.campaignStartSend).toHaveBeenCalledTimes(2));
    expect(conn.query.createCampaign).toHaveBeenCalledTimes(1);
    const [first, second] = conn.query.campaignStartSend.mock.calls.map(([args]) => args);
    expect(first).toEqual(second);
  });

  it("schedules at the chosen local time", async () => {
    const conn = seed();
    mountWindow(conn);
    await walkToWhen();
    choose(/^Schedule/);
    const at = screen.getByLabelText("Send at · America/Phoenix");

    // No way forward until there is a time, and it is still to come.
    expect(barWord()).toBe("Choose a time");
    expect(barActs()).not.toContain("Next");
    fireEvent.change(at, { target: { value: "2020-01-01T09:00" } });
    expect(barWord()).toBe("Choose a time in the future.");
    expect(barActs()).not.toContain("Next");
    fireEvent.change(at, { target: { value: "2031-10-14T09:00" } });
    // Next joined the bar with the time, and a bar that has just changed takes no press for a moment.
    await readTheBar();
    press("Next");
    expect(openStep()).toBe("Review");
    expect(railLine("When").textContent).toContain("On Tue, Oct 14 at 9:00");

    await waitFor(() => expect(barWord()).toBe("Ready to schedule"));
    await readTheBar();
    press("Schedule");
    expect(screen.getByText("Schedule “August copy” for Tue, Oct 14 at 9:00 (America/Phoenix), to 1,204 subscribers?")).toBeTruthy();
    await readTheBar();
    press("Schedule");

    await waitFor(() => expect(conn.query.campaignScheduleSend).toHaveBeenCalledOnce());
    const created = createdCampaign(conn);
    // The time is this browser's 9:00, as an instant.
    expect(created.scheduledAt).toBe("2031-10-14T16:00:00Z");
    expect(conn.query.campaignScheduleSend).toHaveBeenCalledWith({ campaignId: created.campaignId, scheduledAt: "2031-10-14T16:00:00Z" });
    expect(conn.query.campaignStartSend).not.toHaveBeenCalled();
    // Held until it is scheduled, then handed to its page.
    await waitFor(() => expect(barWord()).toBe("Scheduling"));
    expect(pressable()).toEqual([]);
    const row = campaignRow({ id: created.campaignId as string, name: "August copy", audienceId: "a1", templateId: "t1", senderIdentityId: "s1", scheduledAt: "2031-10-14T16:00:00Z" });
    act(() => conn.subscriptions.emit(CAMPAIGN, row, "NODE_CREATED"));
    expect(barWord()).toBe("Scheduling");
    act(() => conn.subscriptions.emit(CAMPAIGN, { ...row, status: "scheduled" }));
    expect(await screen.findByRole("heading", { level: 3, name: "August copy" })).toBeTruthy();
    await waitFor(() => expect(barWord()).toBe("Scheduled"));
  });

  it("starts repeating", async () => {
    // What the cluster's series reads answer: nothing yet, then the series it starts.
    const seriesRows: Record<string, unknown>[] = [];
    const conn = seed({ campaignSeries: seriesRows });
    mountWindow(conn);
    await walkToWhen();
    choose(/^Repeat/);
    chooseOption(screen.getByLabelText("Every"), "2 weeks");
    fireEvent.change(screen.getByLabelText("Starting · America/Phoenix"), { target: { value: "2031-10-14T09:00" } });
    await readTheBar();
    press("Next");
    expect(openStep()).toBe("Review");

    await waitFor(() => expect(barWord()).toBe("Ready to start repeating"));
    // A repeat is not something a draft can hold.
    expect(barDetail()).toBe("A draft doesn't repeat until you start it.");
    await readTheBar();
    press("Start repeating");
    expect(screen.getByText("Send “August copy” to Newsletter every 2 weeks from Tue, Oct 14 at 9:00?")).toBeTruthy();
    await readTheBar();
    press("Start repeating");

    await waitFor(() => expect(conn.query.campaignConfigureSeries).toHaveBeenCalledOnce());
    const created = createdCampaign(conn);
    expect(created.scheduledAt).toBeUndefined();
    expect(conn.query.campaignConfigureSeries).toHaveBeenCalledWith({
      campaignId: created.campaignId,
      action: "save",
      intervalWeeks: 2,
      firstSendAt: "2031-10-14T16:00:00Z",
      timeZone: ZONE,
    });
    expect(conn.query.campaignStartSend).not.toHaveBeenCalled();
    // Held until its series is there, then handed to its page, repeating.
    await waitFor(() => expect(barWord()).toBe("Starting"));
    expect(pressable()).toEqual([]);
    const id = created.campaignId as string;
    await act(async () => conn.subscriptions.emit(CAMPAIGN, campaignRow({ id, name: "August copy", audienceId: "a1", templateId: "t1", senderIdentityId: "s1" }), "NODE_CREATED"));
    // The new draft's series is read: there is none yet, and the wizard still holds.
    await waitFor(() => expect(conn.query.campaignSeriesForCampaign.mock.calls.some(([args]) => (args as { campaignId: string }).campaignId === id)).toBe(true));
    await act(async () => {});
    expect(barWord()).toBe("Starting");
    const started = {
      id: "series",
      sourceCampaignId: id,
      accountId: "self",
      name: "August copy",
      status: "active",
      intervalWeeks: 2,
      timeZone: ZONE,
      anchorAt: "2031-10-14T16:00:00Z",
      nextAt: "2031-10-14T16:00:00Z",
      createdAt: "2027-10-01T12:00:00Z",
      lastError: "",
    };
    seriesRows.push(started);
    await act(async () => conn.subscriptions.emit(SERIES, started));
    expect(await screen.findByRole("heading", { level: 3, name: "August copy" })).toBeTruthy();
    await waitFor(() => expect(barWord()).toBe("Repeating"));
  });

  it("says the refusal of the write it tried last, never an older one", async () => {
    const conn = seed();
    conn.query.campaignStartSend.mockRejectedValueOnce(new Error("no email sender is registered on this node"));
    conn.query.campaignConfigureSeries.mockRejectedValueOnce(new Error("choose a first send in the future, within five years"));
    mountWindow(conn);
    await walkToReview();
    await waitFor(() => expect(barActs()).toContain("Send now"));
    await readTheBar();
    press("Send now");
    await readTheBar();
    press("Send now");
    expect(await screen.findByText("no email sender is registered on this node")).toBeTruthy();

    // Back to When: it repeats instead, and starting it is refused for a reason of its own.
    fireEvent.click(railLine("When"));
    choose(/^Repeat/);
    chooseOption(screen.getByLabelText("Every"), "2 weeks");
    fireEvent.change(screen.getByLabelText("Starting · America/Phoenix"), { target: { value: "2031-10-14T09:00" } });
    await readTheBar();
    press("Next");
    await waitFor(() => expect(barActs()).toContain("Start repeating"));
    await readTheBar();
    press("Start repeating");
    await readTheBar();
    press("Start repeating");
    expect(await screen.findByText("choose a first send in the future, within five years")).toBeTruthy();
    expect(screen.getByText("The campaign was saved as a draft, but repeating didn't start.")).toBeTruthy();
    expect(screen.queryByText("no email sender is registered on this node")).toBeNull();
  });

  it("saves a draft with the time it was meant for, and opens it", async () => {
    const conn = seed();
    mountWindow(conn);
    await walkToWhen();
    choose(/^Schedule/);
    fireEvent.change(screen.getByLabelText("Send at · America/Phoenix"), { target: { value: "2031-10-14T09:00" } });
    await readTheBar();
    press("Next");
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Launch day" } });
    await waitFor(() => expect(barActs()).toContain("Save as draft"));
    await readTheBar();
    press("Save as draft");

    await waitFor(() => expect(conn.query.createCampaign).toHaveBeenCalledOnce());
    const created = createdCampaign(conn);
    expect(created).toMatchObject({ name: "Launch day", scheduledAt: "2031-10-14T16:00:00Z" });
    expect(conn.query.campaignScheduleSend).not.toHaveBeenCalled();
    act(() => conn.subscriptions.emit(CAMPAIGN, campaignRow({ id: created.campaignId as string, name: "Launch day", audienceId: "a1", templateId: "t1" }), "NODE_CREATED"));
    expect(await screen.findByRole("heading", { level: 3, name: "Launch day" })).toBeTruthy();
  });

  it("sends a test to the testing audience, or to me when none is set", async () => {
    // The organization has a testing audience: the test goes there.
    const testing = seed({ testSettings: [{ id: "ts", accountId: "self", audienceId: "a-test", createdAt: "2026-10-01T00:00:00Z" }] });
    const first = mountWindow(testing);
    await walkToReview();
    await waitFor(() => expect(barActs()).toContain("Send a test"));
    await readTheBar();
    press("Send a test");
    await waitFor(() => expect(testing.query.campaignTestAudienceSend).toHaveBeenCalledOnce());
    const created = createdCampaign(testing);
    expect(testing.query.campaignTestAudienceSend.mock.calls[0]![0]).toMatchObject({ campaignId: created.campaignId, audienceId: "a-test", requestId: expect.any(String) });
    expect(await screen.findByText("Test queued for Reviewers.")).toBeTruthy();
    expect(testing.query.campaignTestSend).not.toHaveBeenCalled();
    // The draft exists now, so the floor is the draft's.
    expect(barActs()).toEqual(["Cancel", "Send a test", "Send now"]);
    // A second test sends again and writes no second draft -- once the draft's floor has been read.
    await readTheBar();
    press("Send a test");
    await waitFor(() => expect(testing.query.campaignTestAudienceSend).toHaveBeenCalledTimes(2));
    expect(testing.query.createCampaign).toHaveBeenCalledTimes(1);
    first.view.unmount();

    // None set: to the person's own address, which also says what did not merge.
    const toMe = seed();
    toMe.query.campaignTestSend.mockResolvedValue(rowsResult([{ unresolvedTags: ["{{fields.compnay}}"] }]));
    mountWindow(toMe);
    await walkToReview();
    await waitFor(() => expect(barActs()).toContain("Send a test"));
    await readTheBar();
    press("Send a test");
    await waitFor(() => expect(toMe.query.campaignTestSend).toHaveBeenCalledOnce());
    const draft = createdCampaign(toMe);
    expect(toMe.query.campaignTestSend).toHaveBeenCalledWith({ campaignId: draft.campaignId, to: "owner@example.com" });
    expect(await screen.findByText("Test sent to owner@example.com.")).toBeTruthy();
    expect(screen.getByText("These merge tags didn't resolve: {{fields.compnay}}.")).toBeTruthy();
    expect(toMe.query.campaignTestAudienceSend).not.toHaveBeenCalled();
    expect(toMe.query.createCampaign).toHaveBeenCalledTimes(1);
  });

  it("sends no test to the audience the campaign goes to, and says where it went instead", async () => {
    // The organization's testing audience is the one this campaign is for: a
    // "test" to it would be the send. It goes to the person's own address.
    const conn = seed({ testSettings: [{ id: "ts", accountId: "self", audienceId: "a1", createdAt: "2026-10-01T00:00:00Z" }] });
    mountWindow(conn);
    await walkToReview();
    await waitFor(() => expect(barActs()).toContain("Send a test"));
    await readTheBar();
    press("Send a test");
    await waitFor(() => expect(conn.query.campaignTestSend).toHaveBeenCalledWith({ campaignId: createdCampaign(conn).campaignId, to: "owner@example.com" }));
    expect(conn.query.campaignTestAudienceSend).not.toHaveBeenCalled();
    expect(await screen.findByText("Test sent to owner@example.com.")).toBeTruthy();
  });

  it("points a captured test to Inbox only for people who can open it", async () => {
    const captured = () => seed({ sendingReadiness: [{ ready: true, capture: true, reason: "" }] });
    const owner = mountWindow(captured());
    await walkToReview();
    await waitFor(() => expect(barActs()).toContain("Send a test"));
    await readTheBar();
    press("Send a test");
    const view = await screen.findByRole("button", { name: "View it in Inbox" });
    fireEvent.click(view);
    expect(owner.navigate).toHaveBeenCalledWith("inbox", { fromContent: true });
    owner.view.unmount();

    installSeededAccess("writer");
    mountWindow(captured(), { role: "writer", everyAccount: false, accountIds: ["self"] });
    await walkToReview();
    await waitFor(() => expect(barActs()).toContain("Send a test"));
    await readTheBar();
    press("Send a test");
    expect(await screen.findByText("An owner or developer can read it in Email → Inbox.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "View it in Inbox" })).toBeNull();
  });

  it("writes what Review says when it changed after the draft was made", async () => {
    const conn = seed();
    mountWindow(conn);
    await walkToReview();
    await waitFor(() => expect(barActs()).toContain("Send a test"));
    await readTheBar();
    press("Send a test");
    await waitFor(() => expect(conn.query.campaignTestSend).toHaveBeenCalledOnce());

    // Named after the test: the send carries the name, written to the same draft.
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Spring sale" } });
    await readTheBar();
    press("Send now");
    expect(screen.getByText("Send “Spring sale” to 1,204 subscribers now?")).toBeTruthy();
    await readTheBar();
    press("Send now");
    await waitFor(() => expect(conn.query.campaignStartSend).toHaveBeenCalledOnce());
    const created = createdCampaign(conn);
    expect(conn.query.createCampaign).toHaveBeenCalledTimes(1);
    expect(conn.query.updateCampaign).toHaveBeenCalledOnce();
    expect(conn.query.updateCampaign.mock.calls[0]![0]).toMatchObject({ campaignId: created.campaignId, name: "Spring sale", audienceId: "a1", templateId: "t1" });
  });

  it("after a draft exists, Cancel asks by name and cancels it", async () => {
    const conn = seed();
    mountWindow(conn);
    await walkToReview();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Spring sale" } });
    await waitFor(() => expect(barActs()).toContain("Send a test"));
    await readTheBar();
    press("Send a test");
    await waitFor(() => expect(barActs()).toEqual(["Cancel", "Send a test", "Send now"]));
    await readTheBar();

    press("Cancel");
    expect(screen.getByText("Cancel “Spring sale”?")).toBeTruthy();
    expect(barActs()).toEqual(["Keep", "Cancel campaign"]);
    // The second click of a double-click on Cancel lands on the question, whose
    // Cancel campaign stands where Cancel stood: it answers nothing.
    press("Cancel campaign");
    expect(conn.query.cancelCampaign).not.toHaveBeenCalled();
    expect(screen.getByText("Cancel “Spring sale”?")).toBeTruthy();
    await readTheBar();
    press("Keep");
    expect(barActs()).toEqual(["Cancel", "Send a test", "Send now"]);
    expect(conn.query.cancelCampaign).not.toHaveBeenCalled();

    // Keep put the acts back, and a bar that has just changed takes no press for a moment.
    await readTheBar();
    press("Cancel");
    await readTheBar();
    press("Cancel campaign");
    await waitFor(() => expect(conn.query.cancelCampaign).toHaveBeenCalledWith({ campaignId: createdCampaign(conn).campaignId }));
    // Back on the list.
    expect(await screen.findByRole("heading", { level: 3, name: "Campaigns" })).toBeTruthy();
  });

  it("a draft template blocks sending and says so", async () => {
    const conn = seed();
    mountWindow(conn);
    await walkToReview(/^Autumn draft/);

    await waitFor(() => expect(barWord()).toBe("Publish “Autumn draft” before sending."));
    expect(within(bar()).queryByRole("button", { name: "Send now" })).toBeNull();
    // Said once, in the bar: Review's sentence is the campaign, not a list of its facts.
    expect(screen.getByText("Send “Autumn draft” to Newsletter (1,204 subscribers) as news@acme.com now.")).toBeTruthy();
    expect(document.querySelector(".os-facts")).toBeNull();
    // A draft can still be saved, and a test to me reads a draft template.
    expect(barActs()).toEqual(["Send a test", "Save as draft"]);
    await readTheBar();
    press("Save as draft");
    await waitFor(() => expect(conn.query.createCampaign).toHaveBeenCalledOnce());
    expect(conn.query.campaignStartSend).not.toHaveBeenCalled();
  });

  it("never shows a disabled act", async () => {
    const open = vi.spyOn(window, "open").mockImplementation(() => null);
    try {
      const conn = seed({ audiences: [audienceRow({ id: "a1", name: "Newsletter" })] });
      mountWindow(conn);
      await openWizard();
      await screen.findByRole("radio", { name: /^Newsletter/ });
      expect(deadButtons()).toEqual([]);
      choose(/^Newsletter/);
      expect(deadButtons()).toEqual([]);
      await readTheBar();
      press("Create new template");
      expect(deadButtons()).toEqual([]);
      await readTheBar();
      press("Stop waiting");
      expect(deadButtons()).toEqual([]);
      choose(/^Autumn draft/);
      choose(/^news@acme\.com/);
      choose(/^Schedule/);
      expect(deadButtons()).toEqual([]);
      fireEvent.change(screen.getByLabelText("Send at · America/Phoenix"), { target: { value: "2031-10-14T09:00" } });
      await readTheBar();
      press("Next");
      await waitFor(() => expect(barWord()).toBe("Publish “Autumn draft” before sending."));
      expect(deadButtons()).toEqual([]);

      fireEvent.click(railLine("Template"));
      choose(/^August copy/);
      fireEvent.click(railLine("Review"));
      await waitFor(() => expect(barWord()).toBe("Ready to schedule"));
      expect(deadButtons()).toEqual([]);
      await readTheBar();
      press("Schedule");
      expect(deadButtons()).toEqual([]);
    } finally {
      open.mockRestore();
    }
  });
});
