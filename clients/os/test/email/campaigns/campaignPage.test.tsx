import { act, fireEvent, render, renderHook, screen, waitFor, within } from "@testing-library/react";
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
const { answerLost, useSendControls } = await import("../../../src/apps/email/actions");
const { audienceRow, campaignRow, deliveryRow, fakeConnection, pagedResult, rowsResult, senderRow, templateRow, withSession } = await import("../harness");

// ONE CAMPAIGN'S PAGE (memql#6033, Ruling 7): its facts, what happened when it
// went out, its tests, and ONE bar that carries everything that changes it.
//
// A section appears once it has something to show: a draft has no results, so
// it draws none. A refused read says so in the engine's words and is never a
// zero. Pinned in one zone with no daylight saving.

type Conn = ReturnType<typeof fakeConnection>;

const CAMPAIGN = "v1:campaigns:campaign";
const TEMPLATE = "v1:campaigns:template";
const ZONE = "America/Phoenix";
/** Tue, Oct 14 2031 at 9:00 in Phoenix. */
const OCT_14 = "2031-10-14T16:00:00Z";
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

const AUDIENCES = [audienceRow({ id: "a1", name: "Newsletter" }), audienceRow({ id: "a-test", name: "Reviewers" })];
const TEMPLATES = [templateRow({ id: "t1", name: "August copy" })];
const SENDERS = [senderRow({ id: "s1", address: "news@acme.com", fromName: "Acme News" })];

/** A draft of "August update" to Newsletter, from news@acme.com. */
function draft(over: Parameters<typeof campaignRow>[0] = { id: "c1" }) {
  return campaignRow({ audienceId: "a1", templateId: "t1", senderIdentityId: "s1", ...over });
}

function seed(over: Parameters<typeof fakeConnection>[0] = {}) {
  return fakeConnection({ audiences: AUDIENCES, templates: TEMPLATES, senderIdentities: SENDERS, subscriberCounts: { a1: 1204, "a-test": 3 }, ...over });
}

/**
 * One Email window, as the shell holds it: `navigate` moves it to the tab it
 * names, so a link that opens another tab's page really leaves, and `goto` is
 * the person pressing a tab to come back.
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

async function openCampaign(name = "August update") {
  fireEvent.click(await screen.findByText(name));
  expect(await screen.findByRole("heading", { level: 3, name })).toBeTruthy();
}

const bar = () => screen.getByRole("group", { name: "What you can do with this" });
const barWord = () => bar().querySelector(".os-actbar-word")?.textContent ?? "";
const barDetail = () => bar().querySelector(".os-actbar-detail")?.textContent ?? "";
const barActs = () => within(bar()).queryAllByRole("button").map((button) => button.textContent);
const press = (name: string) => fireEvent.click(within(bar()).getByRole("button", { name }));

/** A fact's value, by its label, in the details the page opens on. */
function fact(label: string, region = screen.getByRole("region", { name: "Campaign details" })): HTMLElement {
  const term = within(region).getByText(label, { selector: "dt" });
  return term.nextElementSibling as HTMLElement;
}

/** The buttons a person could press that do nothing: disabled, and not because they are working. */
function deadButtons(): string[] {
  return screen
    .queryAllByRole("button")
    .filter((button) => (button as HTMLButtonElement).disabled && button.getAttribute("aria-busy") !== "true")
    .map((button) => button.textContent ?? "");
}

/** The acts drawn anywhere but the bar. */
function actsOutsideTheBar(): string[] {
  return screen
    .queryAllByRole("button")
    .filter((button) => button.closest(".os-actbar") === null)
    .map((button) => button.textContent ?? "")
    .filter((text) => /^(Send now|Schedule|Pause|Resume|Cancel|Cancel campaign|Duplicate|Send a test|Edit|Start repeating)$/.test(text));
}

// ---------------------------------------------------------------------------
// What the page opens on
// ---------------------------------------------------------------------------

describe("a campaign's page", () => {
  it("a draft opens on its facts, with no empty results bar", async () => {
    const conn = seed({ campaigns: [draft()] });
    mountWindow(conn);
    await openCampaign();

    expect(fact("Audience").textContent).toBe("Newsletter");
    expect(fact("Template").textContent).toBe("August copy");
    expect(fact("Sender").textContent).toBe("news@acme.com");
    expect(fact("Organization").textContent).toBe("Operator organization");
    expect(fact("When").textContent).toBe("Not scheduled");
    // No reply-to is set, so there is no line for one.
    expect(within(screen.getByRole("region", { name: "Campaign details" })).queryByText("Reply to")).toBeNull();

    // Nothing has gone out: no band, no breakdown, no ledger -- and nothing read for them.
    expect(screen.queryByRole("img")).toBeNull();
    for (const name of ["Results", "Breakdown", "Who got it", "Tests"]) expect(screen.queryByRole("heading", { name })).toBeNull();
    await waitFor(() => expect(barActs()).toEqual(["Edit", "Send a test", "Send now"]));
    expect(barWord()).toBe("Draft");
    expect(conn.query.campaignStats).not.toHaveBeenCalled();
    expect(conn.query.deliveriesForCampaign).not.toHaveBeenCalled();
    expect(deadButtons()).toEqual([]);
  });

  it("its audience and template facts open their pages", async () => {
    const conn = seed({ campaigns: [draft()] });
    const { navigate, goto } = mountWindow(conn);
    await openCampaign();

    fireEvent.click(within(fact("Audience")).getByRole("button", { name: "Newsletter" }));
    expect(navigate).toHaveBeenCalledWith("audiences", { fromContent: true });
    expect(await screen.findByRole("heading", { level: 3, name: "Newsletter" })).toBeTruthy();

    goto("campaigns");
    fireEvent.click(within(fact("Template")).getByRole("button", { name: "August copy" }));
    expect(navigate).toHaveBeenCalledWith("templates", { fromContent: true });
    expect(await screen.findByRole("heading", { level: 3, name: "August copy" })).toBeTruthy();
  });

  it("says when it is scheduled for, and names the sender the cluster sends as", async () => {
    const conn = seed({ campaigns: [draft({ id: "c1", status: "scheduled", scheduledAt: OCT_14, senderIdentityId: "", replyTo: "help@acme.com" })] });
    mountWindow(conn);
    await openCampaign();
    expect(fact("When").textContent).toBe("Tue, Oct 14 at 9:00 (America/Phoenix)");
    expect(fact("Sender").textContent).toBe("This cluster's sender");
    expect(fact("Reply to").textContent).toBe("help@acme.com");
    await waitFor(() => expect(barWord()).toBe("Scheduled"));
    // When it goes is said once: by the fact, with its zone. The bar does not repeat it.
    expect(barDetail()).toBe("");
    expect(screen.getAllByText(/Oct 14 at 9:00/)).toHaveLength(1);
  });

  it("a campaign with no organization says so instead of loading", async () => {
    const conn = seed({ campaigns: [draft({ id: "c1", accountId: "" })] });
    mountWindow(conn);
    await openCampaign();
    expect(await screen.findByText("This campaign has no organization. Edit it to choose one.")).toBeTruthy();
    await waitFor(() => expect(barActs()).toEqual(["Edit"]));
    // Said once, where the way to fix it is: the bar does not say it again.
    expect(barDetail()).toBe("");
    // Nothing is drawn as loading: there is nothing to wait for.
    expect(document.querySelectorAll('[aria-busy="true"]')).toHaveLength(0);
    expect(conn.query.campaignTestAudienceSend).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// The bar: asking, and acting
// ---------------------------------------------------------------------------

describe("acting on a campaign", () => {
  it("asks in the bar, naming the audience size, before sending", async () => {
    // `window.confirm` blocks the whole shell, and a refusal inside a dialog
    // that then closes is a refusal nobody can re-read.
    const conn = seed({ campaigns: [draft()], subscriberCounts: { a1: 4182 } });
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toEqual(["Edit", "Send a test", "Send now"]));
    await readTheBar();

    press("Send now");
    expect(await within(bar()).findByText("Send “August update” to 4,182 subscribers now?")).toBeTruthy();
    expect(barActs()).toEqual(["Not yet", "Send now"]);
    expect(conn.query.campaignStartSend).not.toHaveBeenCalled();
    // Not yet sends nothing and asks nothing -- once the question has been read.
    await readTheBar();
    press("Not yet");
    expect(barActs()).toEqual(["Edit", "Send a test", "Send now"]);
    expect(within(bar()).queryByText(/subscribers now\?$/)).toBeNull();

    // Not yet put the acts back, and a bar that has just changed takes no press for a moment.
    await readTheBar();
    press("Send now");
    await readTheBar();
    press("Send now");
    await waitFor(() => expect(conn.query.campaignStartSend).toHaveBeenCalledWith({ campaignId: "c1" }));
    expect(conn.query.campaignStartSend).toHaveBeenCalledTimes(1);
  });

  it("schedules a draft at its saved time, after asking", async () => {
    const conn = seed({ campaigns: [draft({ id: "c1", scheduledAt: OCT_14 })] });
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toEqual(["Edit", "Send a test", "Schedule"]));
    await readTheBar();
    press("Schedule");
    expect(await within(bar()).findByText("Schedule “August update” for Tue, Oct 14 at 9:00 (America/Phoenix), to 1,204 subscribers?")).toBeTruthy();
    await readTheBar();
    press("Schedule");
    await waitFor(() => expect(conn.query.campaignScheduleSend).toHaveBeenCalledWith({ campaignId: "c1", scheduledAt: OCT_14 }));
  });

  it("cancel asks first and says what stays", async () => {
    const conn = seed({ campaigns: [draft({ id: "c1", status: "scheduled", scheduledAt: OCT_14 })] });
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toEqual(["Cancel", "Send a test", "Send now"]));
    await readTheBar();

    press("Cancel");
    expect(within(bar()).getByText("Cancel “August update”? Nobody will get it, and it stays on the list as cancelled.")).toBeTruthy();
    expect(barActs()).toEqual(["Keep", "Cancel campaign"]);
    // A double-click on Cancel: its second click lands on the question, whose
    // Cancel campaign stands on the right edge where Cancel's row of acts
    // ended. It answers nothing.
    press("Cancel campaign");
    expect(conn.query.cancelCampaign).not.toHaveBeenCalled();
    await readTheBar();
    press("Keep");
    expect(barActs()).toEqual(["Cancel", "Send a test", "Send now"]);
    expect(conn.query.cancelCampaign).not.toHaveBeenCalled();

    await readTheBar();
    press("Cancel");
    await readTheBar();
    press("Cancel campaign");
    await waitFor(() => expect(conn.query.cancelCampaign).toHaveBeenCalledWith({ campaignId: "c1" }));
  });

  it("cancelling a paused send says the mail already sent stays sent", async () => {
    const conn = seed({ campaigns: [draft({ id: "c1", status: "paused", recipientCount: 100, sentCount: 40 })] });
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toEqual(["Cancel", "Send a test", "Resume"]));
    await readTheBar();
    press("Cancel");
    expect(within(bar()).getByText("Cancel “August update”? The 40 already sent stay sent, and nobody else will get it.")).toBeTruthy();
  });

  it("pauses a send and resumes it without a question", async () => {
    const sending = draft({ id: "c1", status: "sending", recipientCount: 100, sentCount: 40 });
    const conn = seed({ campaigns: [sending] });
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toEqual(["Send a test", "Pause"]));
    expect(barWord()).toBe("Sending");
    expect(barDetail()).toBe("40 of 100 sent");

    await readTheBar();
    press("Pause");
    await waitFor(() => expect(conn.query.campaignPauseSend).toHaveBeenCalledWith({ campaignId: "c1" }));
    expect(bar().querySelector(".os-actbar-confirm")).toBeNull();

    act(() => conn.subscriptions.emit(CAMPAIGN, { ...sending, status: "paused" }));
    await waitFor(() => expect(barActs()).toEqual(["Cancel", "Send a test", "Resume"]));
    await readTheBar();
    press("Resume");
    await waitFor(() => expect(conn.query.campaignResumeSend).toHaveBeenCalledWith({ campaignId: "c1" }));
    expect(bar().querySelector(".os-actbar-confirm")).toBeNull();
  });

  it("keeps Resume for a paused send whose template was drafted again after it started", async () => {
    const conn = seed({
      campaigns: [draft({ id: "c1", status: "paused", recipientCount: 100, sentCount: 40 })],
      templates: [templateRow({ id: "t1", name: "August copy", status: "draft" })],
    });
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toEqual(["Cancel", "Send a test", "Resume"]));
    await readTheBar();
    press("Resume");
    await waitFor(() => expect(conn.query.campaignResumeSend).toHaveBeenCalledWith({ campaignId: "c1" }));
  });

  it("duplicate opens a new draft with the same audience, template and sender", async () => {
    const conn = seed({
      campaigns: [draft({ id: "c1", status: "sent", replyTo: "help@acme.com", trackClicks: false, recipientCount: 10, sentCount: 10, completedAt: "2031-10-15T17:30:00Z", scheduledAt: OCT_14 })],
    });
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toEqual(["Send a test", "Duplicate"]));
    await readTheBar();
    press("Duplicate");

    await waitFor(() => expect(conn.query.createCampaign).toHaveBeenCalledOnce());
    const args = conn.query.createCampaign.mock.calls[0]![0];
    expect(args).toMatchObject({
      name: "August update (copy)",
      audienceId: "a1",
      templateId: "t1",
      senderIdentityId: "s1",
      accountId: "self",
      replyTo: "help@acme.com",
      trackOpens: true,
      trackClicks: false,
    });
    expect(args.campaignId).not.toBe("c1");
    // A copy is a new draft: it is not scheduled for the time the original was.
    expect(args.scheduledAt).toBeUndefined();
    expect(args).not.toHaveProperty("ownerUserId");

    // The copy's page opens when its row arrives.
    act(() => conn.subscriptions.emit(CAMPAIGN, draft({ id: args.campaignId as string, name: "August update (copy)" }), "NODE_CREATED"));
    expect(await screen.findByRole("heading", { level: 3, name: "August update (copy)" })).toBeTruthy();
    await waitFor(() => expect(barWord()).toBe("Draft"));
  });

  it("a duplicate tried again after a refusal is the same copy", async () => {
    const conn = seed({ campaigns: [draft({ id: "c1", status: "sent", recipientCount: 10, sentCount: 10 })] });
    conn.query.createCampaign.mockRejectedValueOnce(new Error("Connection lost"));
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toContain("Duplicate"));
    await readTheBar();
    press("Duplicate");
    expect(await screen.findByText("The campaign wasn't duplicated.")).toBeTruthy();
    expect(screen.getByText("Connection lost")).toBeTruthy();
    press("Duplicate");
    await waitFor(() => expect(conn.query.createCampaign).toHaveBeenCalledTimes(2));
    const [first, second] = conn.query.createCampaign.mock.calls.map(([args]) => args.campaignId);
    expect(second).toBe(first);
  });

  it("a duplicate whose answer never came back says it could not be confirmed, and that trying again makes the same copy", async () => {
    const conn = seed({ campaigns: [draft({ id: "c1", status: "sent", recipientCount: 10, sentCount: 10 })] });
    conn.query.createCampaign.mockRejectedValueOnce(lostAnswer());
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toContain("Duplicate"));
    await readTheBar();
    press("Duplicate");
    expect(await screen.findByText("Couldn't confirm the copy.")).toBeTruthy();
    expect(screen.getByText("Try again: it makes the same copy, never a second one.")).toBeTruthy();
    expect(screen.queryByText("The campaign wasn't duplicated.")).toBeNull();
  });

  it("says in the engine's own words why an act was refused", async () => {
    const conn = seed({ campaigns: [draft()] });
    conn.query.campaignStartSend.mockRejectedValueOnce(new Error("no email sender is registered on this node"));
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toContain("Send now"));
    await readTheBar();
    press("Send now");
    await readTheBar();
    press("Send now");
    expect(await screen.findByText("The campaign wasn't sent.")).toBeTruthy();
    expect(screen.getByText("no email sender is registered on this node")).toBeTruthy();
  });

  it("acts live only in the bar", async () => {
    const conn = seed({
      campaigns: [
        draft({ id: "c-draft", name: "Autumn draft" }),
        draft({ id: "c-scheduled", name: "Launch day", status: "scheduled", scheduledAt: OCT_14 }),
        draft({ id: "c-sending", name: "Halfway", status: "sending", recipientCount: 10, sentCount: 4 }),
        draft({ id: "c-paused", name: "On hold", status: "paused", recipientCount: 10, sentCount: 4 }),
        draft({ id: "c-sent", name: "July update", status: "sent", recipientCount: 10, sentCount: 10 }),
      ],
      testRuns: [campaignRow({ id: "run-1", testSourceCampaignId: "c-sent", audienceId: "a-test", status: "sent", recipientCount: 1, sentCount: 1 })],
      deliveriesForCampaign: [deliveryRow({ id: "d1" })],
      campaignStats: [{ sent: 10 }],
    });
    mountWindow(conn);
    const expected: Record<string, string[]> = {
      "Autumn draft": ["Edit", "Send a test", "Send now"],
      "Launch day": ["Cancel", "Send a test", "Send now"],
      Halfway: ["Send a test", "Pause"],
      "On hold": ["Cancel", "Send a test", "Resume"],
      "July update": ["Send a test", "Duplicate"],
    };
    for (const [name, acts] of Object.entries(expected)) {
      await openCampaign(name);
      await waitFor(() => expect(barActs()).toEqual(acts));
      expect(actsOutsideTheBar()).toEqual([]);
      expect(deadButtons()).toEqual([]);
      fireEvent.click(screen.getByRole("button", { name: "Back to Campaigns" }));
      await screen.findByRole("heading", { level: 3, name: "Campaigns" });
    }
  });
});

// ---------------------------------------------------------------------------
// What decides whether it can be sent
// ---------------------------------------------------------------------------

/** An answer that never came back: the stream closed after the write went out, so it may have been done. */
function lostAnswer(): Error {
  return Object.assign(new Error("stream closed"), { reason: "transport" });
}

/** The bar's acts that can be pressed now: not disabled. */
const pressable = () => within(bar()).queryAllByRole("button").filter((button) => !(button as HTMLButtonElement).disabled).map((button) => button.textContent);

describe("a change on its way", () => {
  // AFTER A CONFIRMED PRESS THE ACT STAYS BUSY UNTIL THE CHANGE LANDS. The write
  // returning is not the campaign changing: until its row arrives the bar would
  // read Draft and offer Send now again, and a second press would be refused
  // over a send that started. So the bar says what is happening, holds the act
  // pressed, and offers nothing, until the row's status moves.
  const confirmed: { act: string; confirm?: string; campaign: ReturnType<typeof draft>; word: string; write: "campaignStartSend" | "campaignScheduleSend" | "cancelCampaign"; arrives: string; after: string }[] = [
    { act: "Send now", campaign: draft(), word: "Starting", write: "campaignStartSend", arrives: "sending", after: "Sending" },
    { act: "Schedule", campaign: draft({ id: "c1", scheduledAt: OCT_14 }), word: "Scheduling", write: "campaignScheduleSend", arrives: "scheduled", after: "Scheduled" },
    { act: "Cancel", confirm: "Cancel campaign", campaign: draft({ id: "c1", status: "scheduled", scheduledAt: OCT_14 }), word: "Cancelling", write: "cancelCampaign", arrives: "cancelled", after: "Cancelled" },
  ];
  it.each(confirmed)("$act stays busy until the campaign is $arrives, and offers nothing meanwhile", async ({ act: asked, confirm: answer, campaign, word, write, arrives, after }) => {
    const confirm = answer ?? asked;
    const conn = seed({ campaigns: [campaign] });
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toContain(asked));
    await readTheBar();
    press(asked);
    await readTheBar();
    press(confirm);
    await waitFor(() => expect(conn.query[write]).toHaveBeenCalledOnce());

    // The cluster took it; the row has not arrived. The act it was is held.
    await waitFor(() => expect(barWord()).toBe(word));
    expect(barActs()).toEqual([confirm]);
    expect(pressable()).toEqual([]);
    expect(within(bar()).getByRole("button", { name: confirm }).getAttribute("aria-busy")).toBe("true");

    act(() => conn.subscriptions.emit(CAMPAIGN, { ...campaign, status: arrives }));
    await waitFor(() => expect(barWord()).toBe(after));
    expect(pressable().length).toBeGreaterThan(0);
    expect(conn.query[write]).toHaveBeenCalledOnce();
  });

  it("holds until the write itself has answered, even when the row arrives first", async () => {
    const campaign = draft();
    const conn = seed({ campaigns: [campaign] });
    let answer: () => void = () => {};
    conn.query.campaignStartSend.mockImplementationOnce(() => new Promise((resolve) => (answer = () => resolve(rowsResult([])))));
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toContain("Send now"));
    await readTheBar();
    press("Send now");
    await readTheBar();
    press("Send now");
    await waitFor(() => expect(barWord()).toBe("Starting"));
    act(() => conn.subscriptions.emit(CAMPAIGN, { ...campaign, status: "sending" }));
    // The row moved, but the write has not answered: what it says is still to come.
    expect(barWord()).toBe("Starting");
    expect(pressable()).toEqual([]);
    await act(async () => answer());
    await waitFor(() => expect(barWord()).toBe("Sending"));
  });

  it("a pause stays busy until the send is paused", async () => {
    const sending = draft({ id: "c1", status: "sending", recipientCount: 100, sentCount: 40 });
    const conn = seed({ campaigns: [sending] });
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toEqual(["Send a test", "Pause"]));
    await readTheBar();
    press("Pause");
    await waitFor(() => expect(barWord()).toBe("Pausing"));
    expect(barActs()).toEqual(["Pause"]);
    expect(pressable()).toEqual([]);
    act(() => conn.subscriptions.emit(CAMPAIGN, { ...sending, status: "paused" }));
    await waitFor(() => expect(barActs()).toEqual(["Cancel", "Send a test", "Resume"]));
  });

  it("a change that was taken but never arrives is read again after ten seconds, and the bar lets go", async () => {
    const conn = seed({ campaigns: [draft()] });
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toEqual(["Edit", "Send a test", "Send now"]));
    await readTheBar();
    press("Send now");
    await readTheBar();
    vi.useFakeTimers();
    press("Send now");
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    expect(conn.query.campaignStartSend).toHaveBeenCalledOnce();
    expect(barWord()).toBe("Starting");
    const reads = conn.query.campaigns.mock.calls.length;
    await act(async () => { await vi.advanceTimersByTimeAsync(9_000); });
    expect(barWord()).toBe("Starting");
    await act(async () => { await vi.advanceTimersByTimeAsync(1_000); });
    // The page reads the campaign again itself, and stops holding the bar.
    expect(conn.query.campaigns.mock.calls.length).toBeGreaterThan(reads);
    expect(barWord()).toBe("Draft");
    expect(pressable()).toEqual(["Edit", "Send a test", "Send now"]);
  });

  it("an answer that never came back is not a refusal: it says so, reads the campaign again, and clears when the status moves", async () => {
    const campaign = draft();
    const conn = seed({ campaigns: [campaign] });
    conn.query.campaignStartSend.mockRejectedValueOnce(lostAnswer());
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toContain("Send now"));
    await readTheBar();
    const reads = conn.query.campaigns.mock.calls.length;
    press("Send now");
    await readTheBar();
    press("Send now");

    expect(await screen.findByText("Couldn't confirm the send.")).toBeTruthy();
    expect(screen.getByText("Its status will update here.")).toBeTruthy();
    expect(screen.queryByText("The campaign wasn't sent.")).toBeNull();
    await waitFor(() => expect(conn.query.campaigns.mock.calls.length).toBeGreaterThan(reads));

    // It went: the row arrives, and what could not be confirmed no longer needs saying.
    act(() => conn.subscriptions.emit(CAMPAIGN, { ...campaign, status: "sending" }));
    await waitFor(() => expect(barWord()).toBe("Sending"));
    expect(screen.queryByText("Couldn't confirm the send.")).toBeNull();
  });

  it("a refusal says why in the engine's words, and clears once the status moves", async () => {
    const campaign = draft();
    const conn = seed({ campaigns: [campaign] });
    conn.query.campaignStartSend.mockRejectedValueOnce(new Error("campaign is already sending"));
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toContain("Send now"));
    await readTheBar();
    press("Send now");
    await readTheBar();
    press("Send now");
    expect(await screen.findByText("The campaign wasn't sent.")).toBeTruthy();
    expect(screen.getByText("campaign is already sending")).toBeTruthy();

    act(() => conn.subscriptions.emit(CAMPAIGN, { ...campaign, status: "sending" }));
    await waitFor(() => expect(barWord()).toBe("Sending"));
    expect(screen.queryByText("The campaign wasn't sent.")).toBeNull();
  });
});

describe("what decides whether it can be sent", () => {
  it("a draft template blocks sending until it is published", async () => {
    const template = templateRow({ id: "t1", status: "draft", name: "Client newsletter" });
    const conn = seed({ campaigns: [draft()], templates: [template] });
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barDetail()).toBe("Publish “Client newsletter” before sending."));
    expect(barActs()).toEqual(["Edit", "Send a test"]);

    // The editor can publish on another node: only its row arriving here changes this.
    act(() => conn.subscriptions.emit(TEMPLATE, { ...template, status: "ready", createdAt: "2026-10-02T15:00:00Z" }));
    await waitFor(() => expect(barActs()).toEqual(["Edit", "Send a test", "Send now"]));
    act(() => conn.subscriptions.emit(TEMPLATE, { ...template, createdAt: "2026-10-02T15:01:00Z" }));
    await waitFor(() => expect(barActs()).toEqual(["Edit", "Send a test"]));
    expect(conn.query.campaignStartSend).not.toHaveBeenCalled();
  });

  it.each(["missing", "archived", "other organization"])("offers no send for a %s template", async (condition) => {
    const conn = seed({
      campaigns: [draft()],
      templates: condition === "missing" ? [] : [templateRow({ id: "t1", status: condition === "archived" ? "archived" : "ready", accountId: condition === "other organization" ? "client-b" : "self" })],
    });
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barDetail()).toBe("Choose a published template."));
    expect(barActs()).toEqual(["Edit", "Send a test"]);
  });

  it("an unreadable templates list offers no send, and says so", async () => {
    const conn = seed({ campaigns: [draft()] });
    conn.query.templates.mockRejectedValue(new Error("Template read refused"));
    mountWindow(conn);
    await openCampaign();
    expect(await screen.findByText("This cluster did not return your templates.")).toBeTruthy();
    expect(screen.getByText("Template read refused")).toBeTruthy();
    await waitFor(() => expect(barActs()).toContain("Edit"));
    expect(barActs()).not.toContain("Send now");
    expect(barActs()).not.toContain("Schedule");
  });

  it("says sending isn't set up, with the engine's reason and where to set it up", async () => {
    const conn = seed({ campaigns: [draft()], sendingReadiness: [{ ready: false, reason: "Verify the sending domain first." }] });
    const { navigate } = mountWindow(conn);
    await openCampaign();
    // One sentence (Ruling 6), the engine's reason beneath it, and the way to fix it beside it.
    expect(await screen.findByText("Sending isn't set up for Operator organization.")).toBeTruthy();
    expect(screen.getAllByText(/Sending isn't set up|Set up sending/)).toHaveLength(1);
    expect(barDetail()).toBe("");
    // Nothing can go out, a test included: no act for it, rather than one that does nothing.
    expect(barActs()).toEqual(["Edit"]);
    expect(conn.query.campaignTestAudienceSend).not.toHaveBeenCalled();
    expect(conn.query.campaignTestSend).not.toHaveBeenCalled();
    expect(screen.getByText("Verify the sending domain first.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Open Settings" }));
    expect(navigate).toHaveBeenCalledWith("settings", { fromContent: true });
  });

  it("a sending check that failed says it could not check, not that sending isn't set up", async () => {
    const conn = seed({ campaigns: [draft()], sendingReadiness: new Error("the readiness check timed out") });
    mountWindow(conn);
    await openCampaign();
    expect(await screen.findByText("Couldn't check sending for Operator organization.")).toBeTruthy();
    expect(screen.getAllByText(/Couldn't check sending/)).toHaveLength(1);
    expect(barDetail()).toBe("");
    expect(screen.queryByText(/Sending isn't set up/)).toBeNull();
    expect(screen.getByText("the readiness check timed out")).toBeTruthy();
  });

  it("tells someone who cannot set up sending who can, in the same notice", async () => {
    installSeededAccess("writer");
    const conn = seed({ campaigns: [draft()], sendingReadiness: [{ ready: false, reason: "Verify the sending domain first." }] });
    mountWindow(conn, { role: "writer" });
    await openCampaign();
    expect(await screen.findByText("Sending isn't set up for Operator organization.")).toBeTruthy();
    expect(screen.getByText("An owner or developer sets it up.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Open Settings" })).toBeNull();
  });

  it("does not mistake the cluster's own mailbox report for every organization's sending", async () => {
    const conn = seed({
      campaigns: [draft({ id: "c1", accountId: "client-a" })],
      templates: [templateRow({ id: "t1", accountId: "client-a" })],
      integrationStatus: [{ integrations: [{ name: "email", configured: "no", health: "degraded", mode: "log", detail: "no sender configured" }] }],
    });
    mountWindow(conn);
    await screen.findByText("August update");
    expect(screen.queryByText(/nothing sent from here will arrive|missing what it needs to send mail/)).toBeNull();
    await openCampaign();
    await waitFor(() => expect(barActs()).toContain("Send now"));
    expect(conn.query.campaignSendingReadiness).toHaveBeenCalledWith({ campaignId: "c1" }, expect.anything());
    expect(screen.queryByText(/nothing sent from here will arrive|missing what it needs to send mail/)).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// What happened when it went out
// ---------------------------------------------------------------------------

const STATS = {
  recipients: 100,
  pending: 45,
  sent: 40,
  failed: 5,
  skipped: { total: 10, suppressed: 6, unsubscribed: 3, other: 1 },
  bounces: { hard: 2 },
  complaints: 1,
  unsubscribed: 4,
  // Unique opens hit the read's bound: unmeasured, not zero.
  opens: { total: 90 },
  clicks: { total: 5, unique: 4 },
};

describe("what happened when it went out", () => {
  it("draws results, engagement as a rate of delivered, and an em dash with its reason", async () => {
    const conn = seed({
      campaigns: [
        draft({ id: "c1", status: "sending", recipientCount: 100, sentCount: 40, skippedCount: 10, failedCount: 5, startedAt: "2031-10-14T16:00:00Z" }),
        draft({ id: "c2", name: "Broken send", status: "failed", lastError: "no email sender is registered on this node" }),
      ],
      campaignStats: [STATS],
      deliveriesForCampaign: [deliveryRow({ id: "d1", campaignId: "c1" })],
    });
    mountWindow(conn);
    await openCampaign();

    // The audience, divided by what happened to it, readable without eyes.
    const results = screen.getByRole("region", { name: "Results" });
    expect(within(results).getByRole("img", { name: "100 recipients: 40 sent, 10 skipped, 5 failed, 45 not yet sent." })).toBeTruthy();
    // Opens could not be counted exactly: an em dash, with why. Clicks are a rate of what was delivered.
    await waitFor(() => expect(within(results).getByText("10%")).toBeTruthy());
    expect(within(results).getByText(/count distinct recipients exactly/)).toBeTruthy();
    expect(within(results).getByText("Opened").parentElement?.querySelector(".os-send-engagement-rate")?.textContent).toBe("--");

    const breakdown = screen.getByRole("region", { name: "Breakdown" });
    expect(fact("Hard bounces", breakdown).textContent).toBe("2");
    expect(fact("Spam reports", breakdown).textContent).toBe("1");
    expect(fact("Unsubscribed from this", breakdown).textContent).toBe("4");
    expect(fact("Skipped: on the do-not-mail list", breakdown).textContent).toBe("6");
    // Soft bounces are not counted per campaign, and that is said once, not drawn as a zero.
    expect(within(breakdown).getAllByText(/Soft bounces/)).toHaveLength(1);
    expect(screen.queryByText("Results for this campaign’s audience. Test sends have their own results below.")).toBeNull();
    expect(screen.queryByText(/Updates automatically/)).toBeNull();

    // A send that failed says why, in the engine's own words.
    fireEvent.click(screen.getByRole("button", { name: "Back to Campaigns" }));
    await openCampaign("Broken send");
    await waitFor(() => expect(barWord()).toBe("Failed"));
    expect(barDetail()).toBe("no email sender is registered on this node");
  });

  it("says each number once: the breakdown holds only what the band's legend does not", async () => {
    const conn = seed({
      campaigns: [draft({ id: "c1", status: "sending", recipientCount: 100, sentCount: 40, skippedCount: 10, failedCount: 5, startedAt: "2031-10-14T16:00:00Z" })],
      campaignStats: [STATS],
    });
    mountWindow(conn);
    await openCampaign();

    // The legend beneath the band already says how many were sent, skipped,
    // failed and not yet sent; the breakdown says what it does not.
    const legend = within(screen.getByRole("region", { name: "Results" })).getByRole("list", { name: "What happened to this audience" });
    expect(within(legend).getAllByRole("listitem").map((item) => item.querySelector(".os-send-legend-label")?.textContent)).toEqual(["Sent", "Skipped", "Failed", "Not yet sent"]);
    const breakdown = screen.getByRole("region", { name: "Breakdown" });
    await waitFor(() => expect(breakdown.querySelectorAll("dt").length).toBeGreaterThan(0));
    expect([...breakdown.querySelectorAll("dt")].map((term) => term.textContent)).toEqual([
      "Recipients",
      "Skipped: on the do-not-mail list",
      "Skipped: unsubscribed",
      "Skipped: some other reason",
      "Hard bounces",
      "Spam reports",
      "Unsubscribed from this",
    ]);
    expect(fact("Recipients", breakdown).textContent).toBe("100");
    expect(fact("Skipped: unsubscribed", breakdown).textContent).toBe("3");
    expect(fact("Skipped: some other reason", breakdown).textContent).toBe("1");
    expect(within(breakdown).getAllByText(/Soft bounces/)).toHaveLength(1);
  });

  it("says opens and clicks were not counted when neither was asked for", async () => {
    const conn = seed({
      campaigns: [draft({ id: "c1", status: "sent", trackOpens: false, trackClicks: false, recipientCount: 10, sentCount: 10 })],
      campaignStats: [{ sent: 10 }],
    });
    mountWindow(conn);
    await openCampaign();
    expect(await within(screen.getByRole("region", { name: "Results" })).findByText("Opens and clicks weren't counted for this campaign.")).toBeTruthy();
  });

  it("a refused breakdown read says so, and is not zero", async () => {
    const conn = seed({
      campaigns: [draft({ id: "c1", status: "sent", recipientCount: 10, sentCount: 10 })],
      campaignStats: new Error("reading the breakdown is not yours to do"),
    });
    mountWindow(conn);
    await openCampaign();
    expect(await screen.findByText("This cluster did not return the breakdown.")).toBeTruthy();
    expect(screen.getByText("reading the breakdown is not yours to do")).toBeTruthy();
  });

  it("a refused ledger read is not zero", async () => {
    const conn = seed({
      campaigns: [draft({ id: "c1", status: "sent", recipientCount: 10, sentCount: 10 })],
      deliveriesForCampaign: new Error("reading deliveries is owner and admin only"),
    });
    mountWindow(conn);
    await openCampaign();
    expect(await screen.findByText("This cluster did not return the delivery record.")).toBeTruthy();
    expect(screen.getByText("reading deliveries is owner and admin only")).toBeTruthy();
    expect(screen.queryByText(/silence, not an empty send/)).toBeNull();
    const who = screen.getByRole("region", { name: "Who got it" });
    expect(within(who).queryByRole("button", { name: "Show more" })).toBeNull();
    expect(who.querySelector(".os-subhead-meta")).toBeNull();
  });

  it("refines who got it and loads more", async () => {
    const page1 = [
      deliveryRow({ id: "d4", campaignId: "c1", email: "dana@acme.com", status: "sent", createdAt: "2031-10-14T16:04:00Z" }),
      deliveryRow({ id: "d3", campaignId: "c1", email: "lee@acme.com", status: "skipped", skipReason: "unsubscribed", sentAt: "", createdAt: "2031-10-14T16:03:00Z" }),
    ];
    const page2 = [
      deliveryRow({ id: "d2", campaignId: "c1", email: "sam@acme.com", status: "failed", lastError: "mailbox full", sentAt: "", createdAt: "2031-10-14T16:02:00Z" }),
      deliveryRow({ id: "d1", campaignId: "c1", email: "kim@acme.com", status: "pending", sentAt: "", createdAt: "2031-10-14T16:01:00Z" }),
    ];
    const conn = seed({
      campaigns: [draft({ id: "c1", status: "sending", recipientCount: 4, sentCount: 1, skippedCount: 1, failedCount: 1 })],
      campaignStats: [{ recipients: 4, pending: 1, sent: 1, failed: 1, skipped: { total: 1 } }],
    });
    conn.query.deliveriesForCampaign.mockImplementation(async (_args: unknown, opts?: unknown) =>
      (opts as { cursor?: string } | undefined)?.cursor === "after-d3" ? rowsResult(page2) : pagedResult(page1, "after-d3"),
    );
    mountWindow(conn);
    await openCampaign();

    const who = screen.getByRole("region", { name: "Who got it" });
    expect(await within(who).findByText("dana@acme.com")).toBeTruthy();
    // More exist than were read: the heading says how many of how many.
    await waitFor(() => expect(who.querySelector(".os-subhead-meta")?.textContent).toBe("Latest 2 of 4"));
    // Each outcome in the app's words, never the engine's.
    expect(within(who).getByText("Sent")).toBeTruthy();
    expect(within(who).getByText("They unsubscribed")).toBeTruthy();

    fireEvent.click(within(who).getByRole("button", { name: "Show more" }));
    expect(await within(who).findByText("sam@acme.com")).toBeTruthy();
    expect(within(who).getByText("kim@acme.com")).toBeTruthy();
    expect(within(who).getByText("mailbox full")).toBeTruthy();
    expect(within(who).getByText("Not yet sent")).toBeTruthy();
    expect(conn.query.deliveriesForCampaign).toHaveBeenLastCalledWith({ campaignId: "c1" }, expect.objectContaining({ cursor: "after-d3" }));
    // Everything is read now: no more to show, and no count of what is all there.
    expect(within(who).queryByRole("button", { name: "Show more" })).toBeNull();
    expect(who.querySelector(".os-subhead-meta")).toBeNull();

    // Refine asks of the rows already read: an address, then an outcome.
    fireEvent.click(within(who).getByRole("button", { name: "Refine recipients" }));
    fireEvent.change(within(who).getByLabelText("Search"), { target: { value: "sam" } });
    expect(within(who).getByText("sam@acme.com")).toBeTruthy();
    expect(within(who).queryByText("dana@acme.com")).toBeNull();
    chooseOption(within(who).getByLabelText("Delivery"), "Skipped");
    expect(within(who).getByText("No match among the loaded recipients.")).toBeTruthy();
  });

  it("keeps who got it and the breakdown current without a refresh button", async () => {
    const conn = seed({
      campaigns: [draft({ id: "c1", status: "sent", recipientCount: 10, sentCount: 10 })],
      deliveriesForCampaign: [deliveryRow({ id: "d1", campaignId: "c1" })],
      campaignStats: [{ sent: 10, opens: { unique: 1, total: 1 } }],
    });
    mountWindow(conn);
    await openCampaign();
    const results = screen.getByRole("region", { name: "Results" });
    expect(await within(results).findByText("10%")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /read again|re-read|refresh/i })).toBeNull();
    vi.useFakeTimers();
    await act(async () => window.dispatchEvent(new Event("focus")));
    conn.query.deliveriesForCampaign.mockResolvedValue(rowsResult([deliveryRow({ id: "d2", campaignId: "c1", email: "new-result@example.test", status: "skipped" })]));
    conn.query.campaignStats.mockResolvedValue(rowsResult([{ sent: 10, opens: { unique: 2, total: 2 } }]));
    await act(async () => { await vi.advanceTimersByTimeAsync(5_000); });
    expect(screen.getByText("new-result@example.test")).toBeTruthy();
    // A row the newest page no longer names is still a row that was read: it stays.
    expect(screen.getByText("dana@acme.com")).toBeTruthy();
    expect(within(results).getByText("20%")).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe("each thing said once, where it can be read", () => {
  it("a repeating draft's schedule is said by its bar, not again in its facts", async () => {
    const conn = seed({
      campaigns: [draft()],
      campaignSeries: [{ id: "series", sourceCampaignId: "c1", accountId: "self", name: "August update", status: "active", intervalWeeks: 2, timeZone: ZONE, anchorAt: OCT_14, nextAt: OCT_14, createdAt: "2027-10-01T12:00:00Z", lastError: "" }],
    });
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barDetail()).toBe("Every 2 weeks · next Tue, Oct 14 at 9:00 (America/Phoenix)"));
    const details = screen.getByRole("region", { name: "Campaign details" });
    expect(within(details).queryByText("When", { selector: "dt" })).toBeNull();
    expect(screen.getAllByText(/Every 2 weeks/)).toHaveLength(1);
  });

  it("a paused repeat says how often and from when once, in its facts", async () => {
    const conn = seed({
      campaigns: [draft()],
      campaignSeries: [{ id: "series", sourceCampaignId: "c1", accountId: "self", name: "August update", status: "paused", intervalWeeks: 2, timeZone: ZONE, anchorAt: OCT_14, nextAt: OCT_14, createdAt: "2027-10-01T12:00:00Z", lastError: "" }],
    });
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barWord()).toBe("Repeating paused"));
    expect(barDetail()).toBe("");
    expect(fact("When").textContent).toBe("Every 2 weeks from Tue, Oct 14 at 9:00 (America/Phoenix)");
    expect(screen.getAllByText(/Every 2 weeks/)).toHaveLength(1);
  });

  it("a stopped series' bar says why it stopped, so its schedule is in its facts", async () => {
    const reason = "Publish the template before resuming this schedule.";
    const conn = seed({
      campaigns: [draft()],
      campaignSeries: [{ id: "series", sourceCampaignId: "c1", accountId: "self", name: "August update", status: "blocked", intervalWeeks: 2, timeZone: ZONE, anchorAt: OCT_14, nextAt: OCT_14, createdAt: "2027-10-01T12:00:00Z", lastError: reason }],
    });
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barDetail()).toBe(reason));
    expect(fact("When").textContent).toBe("Every 2 weeks from Tue, Oct 14 at 9:00 (America/Phoenix)");
  });

  it("the breakdown's recipients are the band's: the roster frozen for the send, not the audience today", async () => {
    const conn = seed({
      campaigns: [draft({ id: "c1", status: "sent", recipientCount: 1204, sentCount: 1180, skippedCount: 24 })],
      // The audience has grown since: the server counts today's roster.
      campaignStats: [{ ...STATS, recipients: 1250 }],
    });
    mountWindow(conn);
    await openCampaign();
    const results = screen.getByRole("region", { name: "Results" });
    expect(within(results).getByRole("img").getAttribute("aria-label")).toContain("1204 recipients");
    const breakdown = screen.getByRole("region", { name: "Breakdown" });
    await waitFor(() => expect(breakdown.querySelectorAll("dt").length).toBeGreaterThan(0));
    expect(fact("Recipients", breakdown).textContent).toBe("1,204");
  });

  it("a failed send says why in a notice too, where the whole sentence can be read", async () => {
    const reason = "The sending service refused the sender address because its domain is not verified.";
    const conn = seed({ campaigns: [draft({ id: "c1", status: "failed", lastError: reason })] });
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barWord()).toBe("Failed"));
    const notice = (await screen.findByText("The send failed.")).closest(".os-notice") as HTMLElement;
    expect(within(notice).getByText(reason)).toBeTruthy();
  });

  it("the latest test's figures are its band's: its row does not say them again", async () => {
    const latest = campaignRow({ id: "run-2", name: "August update · Test", testSourceCampaignId: "c1", audienceId: "a-test", status: "sent", recipientCount: 3, sentCount: 2, skippedCount: 1, startedAt: "2031-10-14T17:00:00Z" });
    const older = campaignRow({ id: "run-1", name: "August update · Test", testSourceCampaignId: "c1", audienceId: "a-test", status: "sent", recipientCount: 3, sentCount: 3, startedAt: "2031-10-13T17:00:00Z" });
    const conn = seed({ campaigns: [draft({ id: "c1", status: "sent", recipientCount: 1, sentCount: 1 })], testRuns: [latest, older], campaignStats: [{ sent: 1 }] });
    mountWindow(conn);
    await openCampaign();
    const band = await screen.findByRole("group", { name: "Test results" });
    expect(within(band).getByRole("img").getAttribute("aria-label")).toContain("2 sent, 1 skipped");
    const rows = within(screen.getByRole("list", { name: "Test runs" })).getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(within(rows[0]!).queryByText(/sent, .* skipped/)).toBeNull();
    expect(within(rows[1]!).getByText("3 sent, 0 skipped, 0 failed")).toBeTruthy();
  });
});

describe("testing a campaign", () => {
  it("shows the latest test and opens a run on its own page", async () => {
    const first = campaignRow({ id: "run-1", name: "August update · Test", testSourceCampaignId: "c1", audienceId: "a-test", status: "sent", recipientCount: 1, sentCount: 1, startedAt: "2031-10-14T16:00:00Z" });
    const conn = seed({
      campaigns: [draft({ id: "c1", status: "sent", recipientCount: 1, sentCount: 1 })],
      testRuns: [first],
      campaignStats: [{ sent: 1 }],
    });
    mountWindow(conn);
    await openCampaign();

    const latest = await screen.findByRole("group", { name: "Test results" });
    expect(within(latest).getByRole("img").getAttribute("aria-label")).toContain("1 sent, 0 skipped");
    vi.useFakeTimers();
    // A new test lands on the server, with no event on this browser's node.
    const second = { ...first, id: "run-2", sentCount: 0, skippedCount: 1, startedAt: "2031-10-14T17:00:00Z" };
    conn.query.campaignTestRuns.mockResolvedValue(rowsResult([second, first]));
    await act(async () => window.dispatchEvent(new Event("focus")));
    await act(async () => { await vi.advanceTimersByTimeAsync(5_000); });
    expect(within(screen.getByRole("group", { name: "Test results" })).getByRole("img").getAttribute("aria-label")).toContain("0 sent, 1 skipped");
    // The campaign's own results are its own.
    const own = within(screen.getByRole("region", { name: "Results" })).getByRole("img");
    expect(own.getAttribute("aria-label")).toContain("1 sent, 0 skipped");
    vi.useRealTimers();

    // A run opens on its own page, and reads its own deliveries.
    const runs = screen.getByRole("list", { name: "Test runs" });
    expect(within(runs).getAllByRole("button")).toHaveLength(2);
    fireEvent.click(within(runs).getAllByRole("button")[0]!);
    expect(await screen.findByRole("heading", { level: 3, name: "Test on Tue, Oct 14 at 10:00" })).toBeTruthy();
    await waitFor(() => expect(conn.query.deliveriesForCampaign).toHaveBeenCalledWith({ campaignId: "run-2" }, expect.anything()));
    expect(screen.queryByRole("heading", { level: 3, name: "August update" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Back to August update" }));
    expect(await screen.findByRole("heading", { level: 3, name: "August update" })).toBeTruthy();
  });

  it("a test to my own address lists unresolved merge tags", async () => {
    const conn = seed({ campaigns: [draft()] });
    conn.query.campaignTestSend.mockResolvedValue(rowsResult([{ unresolvedTags: ["{{fields.compnay}}"] }]));
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toContain("Send a test"));
    await readTheBar();
    press("Send a test");
    await waitFor(() => expect(conn.query.campaignTestSend).toHaveBeenCalledWith({ campaignId: "c1", to: "owner@example.com" }));
    expect(await screen.findByText("Test sent to owner@example.com.")).toBeTruthy();
    expect(screen.getByText("These merge tags didn't resolve: {{fields.compnay}}.")).toBeTruthy();
    expect(conn.query.campaignTestAudienceSend).not.toHaveBeenCalled();
  });

  it("a test to my own address whose answer never came back may have gone, and says so", async () => {
    const conn = seed({ campaigns: [draft()] });
    conn.query.campaignTestSend.mockRejectedValueOnce(lostAnswer());
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toContain("Send a test"));
    await readTheBar();
    press("Send a test");
    expect(await screen.findByText("Couldn't confirm the test was sent.")).toBeTruthy();
    expect(screen.queryByText("The test wasn't sent.")).toBeNull();
  });

  it.each(["sending", "paused", "sent", "scheduled"])("tests a %s campaign at its testing audience and recovers an uncertain response", async (status) => {
    const conn = seed({
      campaigns: [draft({ id: "c1", status, scheduledAt: status === "scheduled" ? OCT_14 : "", recipientCount: 10, sentCount: 4 })],
      testSettings: [{ id: "settings", accountId: "self", audienceId: "a-test" }],
      testRuns: [campaignRow({ id: "run-previous", testSourceCampaignId: "c1", audienceId: "a-test", status: "sent", recipientCount: 3, sentCount: 2, skippedCount: 1 })],
    });
    conn.query.campaignTestAudienceSend.mockRejectedValueOnce(new Error("Connection lost"));
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toContain("Send a test"));
    await readTheBar();
    press("Send a test");
    expect(await screen.findByText("Connection lost")).toBeTruthy();
    expect(screen.getByText("The test wasn't confirmed.")).toBeTruthy();
    press("Send a test");
    expect(await screen.findByText("Test queued for Reviewers.")).toBeTruthy();
    // The same request, so the run it may already have made is the run it gets.
    expect(conn.query.campaignTestAudienceSend.mock.calls[0]).toEqual(conn.query.campaignTestAudienceSend.mock.calls[1]);
    expect(conn.query.campaignTestAudienceSend.mock.calls[0]![0]).toMatchObject({ campaignId: "c1", audienceId: "a-test" });
    expect(within(await screen.findByRole("group", { name: "Test results" })).getByRole("img").getAttribute("aria-label")).toContain("2 sent, 1 skipped, 0 failed");
    expect(conn.query.campaignStartSend).not.toHaveBeenCalled();
    expect(conn.query.campaignConfigureSeries).not.toHaveBeenCalled();
  });

  it("a testing audience that is the campaign's own is not used: the test comes to me", async () => {
    const conn = seed({ campaigns: [draft()], testSettings: [{ id: "settings", accountId: "self", audienceId: "a1" }] });
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toContain("Send a test"));
    await readTheBar();
    press("Send a test");
    await waitFor(() => expect(conn.query.campaignTestSend).toHaveBeenCalledWith({ campaignId: "c1", to: "owner@example.com" }));
    expect(conn.query.campaignTestAudienceSend).not.toHaveBeenCalled();
  });

  it("offers no test and draws no warning while the testing audience is still being read", async () => {
    const conn = seed({ campaigns: [draft()] });
    let release!: () => void;
    const gate = new Promise<void>((done) => { release = done; });
    const answer = conn.query.campaignTestSettings.getMockImplementation()!;
    conn.query.campaignTestSettings.mockImplementation(async (args, opts) => { await gate; return answer(args, opts); });
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toEqual(["Edit", "Send now"]));
    expect(screen.queryByText(/testing audience/i)).toBeNull();
    await act(async () => { release(); });
    await waitFor(() => expect(barActs()).toEqual(["Edit", "Send a test", "Send now"]));
  });

  it("says when the testing audience can't be read, and never tests the audience it can no longer confirm", async () => {
    const conn = seed({
      campaigns: [draft({ id: "c1", accountId: "org" })],
      templates: [templateRow({ id: "t1", accountId: "org" })],
      audiences: [audienceRow({ id: "a1", name: "Newsletter", accountId: "org" }), audienceRow({ id: "a-test", name: "Reviewers", accountId: "org" })],
      testSettings: [{ id: "settings", accountId: "org", audienceId: "a-test" }],
    });
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toContain("Send a test"));

    // Another organization's campaign arrives, the feed reads again, and this time the cluster refuses.
    conn.query.campaignTestSettings.mockRejectedValue(new Error("Reading testing settings is not yours to do."));
    await act(async () => { conn.subscriptions.emit(CAMPAIGN, campaignRow({ id: "c2", name: "Elsewhere", accountId: "other" }), "NODE_CREATED"); });
    expect(await screen.findByText("This cluster did not return the testing audience.")).toBeTruthy();
    expect(screen.getByText("Reading testing settings is not yours to do.")).toBeTruthy();
    // The refusal changed what the bar offers, and a bar that has just changed takes no press for a moment.
    await readTheBar();
    press("Send a test");
    await waitFor(() => expect(conn.query.campaignTestSend).toHaveBeenCalledWith({ campaignId: "c1", to: "owner@example.com" }));
    expect(conn.query.campaignTestAudienceSend).not.toHaveBeenCalled();
  });

  it("says when the audiences can't be read rather than asking for a testing audience", async () => {
    const conn = seed({ campaigns: [draft()], testSettings: [{ id: "settings", accountId: "self", audienceId: "a-test" }] });
    conn.query.audiences.mockRejectedValue(new Error("Audience read refused"));
    mountWindow(conn);
    await openCampaign();
    expect(await screen.findByText("This cluster did not return your audiences.")).toBeTruthy();
    expect(screen.getByText("Audience read refused")).toBeTruthy();
    expect(screen.queryByText(/Choose a testing audience/)).toBeNull();
  });

  it("captured mail points to Inbox only for people who can open it", async () => {
    const captured = () => seed({ campaigns: [draft()], sendingReadiness: [{ ready: true, capture: true, reason: "" }] });
    const owner = mountWindow(captured());
    await openCampaign();
    await waitFor(() => expect(barActs()).toContain("Send a test"));
    await readTheBar();
    // Before anything goes, the question says where it goes.
    press("Send now");
    expect(await within(bar()).findByText("This cluster keeps email in Inbox instead of delivering it.")).toBeTruthy();
    await readTheBar();
    press("Not yet");
    // Not yet put the acts back, and a bar that has just changed takes no press for a moment.
    await readTheBar();
    press("Send a test");
    fireEvent.click(await screen.findByRole("button", { name: "View it in Inbox" }));
    expect(owner.navigate).toHaveBeenCalledWith("inbox", { fromContent: true });
    owner.view.unmount();

    installSeededAccess("writer");
    mountWindow(captured(), { role: "writer", everyAccount: false, accountIds: ["self"] });
    await openCampaign();
    await waitFor(() => expect(barActs()).toContain("Send a test"));
    await readTheBar();
    press("Send a test");
    expect(await screen.findByText("An owner or developer can read it in Email → Inbox.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "View it in Inbox" })).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// The send controls' one refusal
// ---------------------------------------------------------------------------

describe("the send controls", () => {
  it("say the refusal of the act pressed last, never an older one", async () => {
    const conn = fakeConnection();
    h.connection = conn;
    conn.query.campaignStartSend.mockRejectedValueOnce(new Error("the template is not marked ready"));
    conn.query.cancelCampaign.mockRejectedValueOnce(new Error("a sent campaign cannot be cancelled"));
    const { result } = renderHook(() => useSendControls(), { wrapper: ({ children }) => withSession(children) });

    await act(async () => { await result.current.start("c1"); });
    expect(result.current.error).toBe("the template is not marked ready");
    await act(async () => { await result.current.cancel("c1"); });
    expect(result.current.error).toBe("a sent campaign cannot be cancelled");
    // A later act that worked leaves nothing to say, whatever an earlier one said.
    await act(async () => { await result.current.pause("c1"); });
    expect(result.current.error).toBe("");
  });

  it("tells an answer that never came back from a refusal, and from a request that never left", () => {
    // Out, and no answer: it may have been done.
    expect(answerLost(Object.assign(new Error("stream closed"), { reason: "transport" }))).toBe(true);
    expect(answerLost(Object.assign(new Error("dispatcher stopped"), { reason: "closed" }))).toBe(true);
    // Never out: nothing was done.
    expect(answerLost(Object.assign(new Error("send failed: socket not open (readyState=3)"), { reason: "transport" }))).toBe(false);
    // The engine's own sentence: it was not done.
    expect(answerLost(new Error("campaign is already sending"))).toBe(false);
    expect(answerLost("not an error")).toBe(false);
  });
});
