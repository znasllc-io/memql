import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterAll, beforeAll, describe, expect, it, vi } from "vitest";

import { chooseOption } from "../../selectControl";

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
const { localToInstant } = await import("../../../src/apps/email/when");
const { audienceRow, campaignRow, fakeConnection, rowsResult, withSession } = await import("../harness");

// THE CAMPAIGNS LIST (memql#6033): every campaign, finished ones included and
// dimmed, each with its audience, its state in one word and the time that
// matters to it; asked questions of through Refine; counted only once its read
// has settled.

type Conn = ReturnType<typeof fakeConnection>;

const CAMPAIGN = "v1:campaigns:campaign";
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

function memoryStore(over: Record<string, unknown> = {}) {
  const bag = new Map<string, string>();
  const store = new LocalEmailSettingsStore({
    getItem: (k: string) => bag.get(k) ?? null,
    setItem: (k: string, v: string) => void bag.set(k, v),
  });
  if (Object.keys(over).length > 0) store.save({ ...store.load(), ...over });
  return store;
}

function mount(connection: Conn) {
  h.connection = connection;
  const view = render(
    withSession(
      <EmailApp sectionId="campaigns" navigate={vi.fn()} askContext={() => {}} store={memoryStore()} uploads={{ upload: vi.fn() }} />,
    ),
  );
  return { view };
}

/** The list row a name is drawn in. */
const rowOf = (name: string) => screen.getByText(name).closest(".os-record-row") as HTMLElement;
/** The live-list item (where the arrival cue is drawn) a name is drawn in. */
const itemOf = (name: string) => screen.getByText(name).closest(".os-livelist-row") as HTMLElement;
const headCount = (view: ReturnType<typeof render>) => view.container.querySelector(".os-head-meta")?.textContent ?? null;

const AUDIENCES = [
  audienceRow({ id: "a1", name: "Newsletter" }),
  audienceRow({ id: "a2", name: "Signups" }),
];

describe("the campaigns list", () => {
  it("lists every campaign with its audience, state and time, finished ones dimmed", async () => {
    const conn = fakeConnection({
      audiences: AUDIENCES,
      campaigns: [
        campaignRow({ id: "c-draft", name: "Autumn draft", audienceId: "a2" }),
        campaignRow({ id: "c-scheduled", name: "Launch day", audienceId: "a1", status: "scheduled", scheduledAt: localToInstant("2031-10-14T09:00")! }),
        campaignRow({ id: "c-sending", name: "August update", audienceId: "a1", status: "sending", recipientCount: 1250, sentCount: 1204 }),
        campaignRow({ id: "c-paused", name: "Halfway", audienceId: "a1", status: "paused", recipientCount: 100, sentCount: 40 }),
        campaignRow({ id: "c-sent", name: "July update", audienceId: "a1", status: "sent", completedAt: localToInstant("2026-10-03T12:00")! }),
        campaignRow({ id: "c-failed", name: "Broken send", audienceId: "a1", status: "failed", lastError: "the sending service refused the message" }),
        campaignRow({ id: "c-cancelled", name: "Called off", audienceId: "a1", status: "cancelled" }),
        campaignRow({ id: "c-repeat", name: "Weekly digest", audienceId: "a1" }),
      ],
      campaignSeries: [
        { id: "series-1", sourceCampaignId: "c-repeat", status: "active", intervalWeeks: 1, timeZone: ZONE, nextAt: localToInstant("2031-10-21T09:00")!, createdAt: "2026-10-01T00:00:00Z" },
      ],
    });
    mount(conn);
    await screen.findByText("Autumn draft");

    // EVERY campaign is listed: a finished one is a Status facet, not a preference.
    for (const name of ["Autumn draft", "Launch day", "August update", "Halfway", "July update", "Broken send", "Called off", "Weekly digest"]) {
      expect(screen.getByText(name)).toBeTruthy();
    }
    // Seeded unfiltered: narrowing is asked of the rows already here.
    expect(conn.query.campaigns).toHaveBeenCalledWith({}, expect.anything());

    const state = (name: string) => within(rowOf(name)).queryByText(/^(Draft|Scheduled|Sending|Paused|Sent|Cancelled|Failed|Repeating)$/)?.textContent ?? "";
    const tone = (name: string) => rowOf(name).querySelector(".os-record-status")?.getAttribute("data-tone");

    // The audience is the line under the name.
    expect(within(rowOf("Autumn draft")).getByText("Signups")).toBeTruthy();
    expect(within(rowOf("August update")).getByText("Newsletter")).toBeTruthy();

    expect(state("Autumn draft")).toBe("Draft");
    expect(within(rowOf("Launch day")).getByText("Scheduled Oct 14, 9:00")).toBeTruthy();
    expect(state("Launch day")).toBe("Scheduled");
    expect(within(rowOf("August update")).getByText("1,204 of 1,250 sent")).toBeTruthy();
    expect(state("August update")).toBe("Sending");
    expect(tone("August update")).toBe("accent");
    expect(within(rowOf("Halfway")).getByText("40 of 100 sent")).toBeTruthy();
    expect(state("Halfway")).toBe("Paused");
    expect(within(rowOf("July update")).getByText("Sent Oct 3")).toBeTruthy();
    expect(state("July update")).toBe("Sent");
    expect(state("Broken send")).toBe("Failed");
    expect(tone("Broken send")).toBe("warn");
    expect(state("Called off")).toBe("Cancelled");
    // A draft a series repeats from reads as the series does.
    await waitFor(() => expect(state("Weekly digest")).toBe("Repeating"));
    expect(within(rowOf("Weekly digest")).getByText("Next Oct 21, 9:00")).toBeTruthy();

    // Finished campaigns keep their facts and lose their ink.
    for (const name of ["July update", "Broken send", "Called off"]) expect(rowOf(name).getAttribute("data-dim")).toBe("true");
    for (const name of ["Autumn draft", "Launch day", "August update", "Halfway"]) expect(rowOf(name).getAttribute("data-dim")).toBeNull();

    // No status in the engine's own words anywhere on the list.
    expect(screen.queryByText("sending")).toBeNull();
    expect(screen.queryByText("problem")).toBeNull();
  });

  it("refines by name and by status", async () => {
    const conn = fakeConnection({
      audiences: AUDIENCES,
      campaigns: [
        campaignRow({ id: "c1", name: "August update", audienceId: "a1" }),
        campaignRow({ id: "c2", name: "July update", audienceId: "a1", status: "sent" }),
        campaignRow({ id: "c3", name: "Welcome", audienceId: "a2", status: "sending", recipientCount: 10, sentCount: 1 }),
      ],
    });
    const { view } = mount(conn);
    await screen.findByText("August update");
    await waitFor(() => expect(headCount(view)).toBe("3"));

    fireEvent.click(screen.getByRole("button", { name: "Refine campaigns" }));
    // By name.
    fireEvent.change(screen.getByLabelText("Search"), { target: { value: "update" } });
    expect(screen.getByText("August update")).toBeTruthy();
    expect(screen.getByText("July update")).toBeTruthy();
    expect(screen.queryByText("Welcome")).toBeNull();
    expect(headCount(view)).toBe("2");
    // By audience.
    fireEvent.change(screen.getByLabelText("Search"), { target: { value: "signups" } });
    expect(screen.getByText("Welcome")).toBeTruthy();
    expect(screen.queryByText("August update")).toBeNull();
    fireEvent.change(screen.getByLabelText("Search"), { target: { value: "" } });

    // By status: In progress is everything not yet finished.
    chooseOption(screen.getByLabelText("Status"), "In progress");
    expect(screen.getByText("August update")).toBeTruthy();
    expect(screen.getByText("Welcome")).toBeTruthy();
    expect(screen.queryByText("July update")).toBeNull();
    chooseOption(screen.getByLabelText("Status"), "Sent");
    expect(screen.getByText("July update")).toBeTruthy();
    expect(screen.queryByText("August update")).toBeNull();
    expect(headCount(view)).toBe("1");

    // The constraint stands beside the control, and removing it is one press.
    fireEvent.click(screen.getByRole("button", { name: "Remove Sent" }));
    expect(screen.getByText("August update")).toBeTruthy();
    expect(headCount(view)).toBe("3");
  });

  it("counts only after its read settles, and a denied read is not zero", async () => {
    const pending = fakeConnection({ campaigns: [campaignRow({ id: "c1", name: "Counted campaign" })] });
    let release!: () => void;
    const gate = new Promise<void>((done) => { release = done; });
    const answer = pending.query.campaigns.getMockImplementation()!;
    pending.query.campaigns.mockImplementation(async (args, opts) => { await gate; return answer(args, opts); });
    const first = mount(pending);
    // Reading: the shape of a list, and no count.
    expect(first.view.container.querySelector(".os-record-skeleton")).not.toBeNull();
    expect(headCount(first.view)).toBeNull();
    await act(async () => { release(); });
    await waitFor(() => expect(headCount(first.view)).toBe("1"));
    first.view.unmount();

    const denied = fakeConnection();
    denied.query.campaigns.mockRejectedValue(new Error("permission denied"));
    const second = mount(denied);
    expect(await screen.findByText("This cluster did not return your campaigns.")).toBeTruthy();
    expect(screen.getByText("Nothing below is current.")).toBeTruthy();
    expect(screen.getByText(/permission denied/)).toBeTruthy();
    expect(headCount(second.view)).toBeNull();
    // A refusal is not an empty list.
    expect(screen.queryByText("No campaigns yet")).toBeNull();

    // Try again reads again.
    denied.query.campaigns.mockResolvedValue(rowsResult([campaignRow({ id: "c1", name: "Back again" })]));
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText("Back again")).toBeTruthy();
  });

  it("stays silent while send counters tick and rings on a rename", async () => {
    const sending = campaignRow({ id: "c1", name: "August update", status: "sending", recipientCount: 100, sentCount: 10 });
    const conn = fakeConnection({ campaigns: [sending] });
    mount(conn);
    await screen.findByText("August update");

    // A drain tick: only the counters move. They re-render, and they do not ring.
    await act(async () => {
      conn.subscriptions.emit(CAMPAIGN, { ...sending, sentCount: 40, skippedCount: 3, failedCount: 1 });
    });
    expect(itemOf("August update").getAttribute("data-arrival")).toBeNull();
    expect(screen.getByText("40 of 100 sent")).toBeTruthy();

    // A rename is news.
    await act(async () => {
      conn.subscriptions.emit(CAMPAIGN, { ...sending, name: "September update", sentCount: 41 });
    });
    expect(itemOf("September update").getAttribute("data-arrival")).toBe("updated");

    // A campaign made while somebody watches arrives with the new tick, once.
    await act(async () => {
      conn.subscriptions.emit(CAMPAIGN, campaignRow({ id: "c2", name: "Brand new" }), "NODE_CREATED");
    });
    const arrived = itemOf("Brand new");
    expect(arrived.getAttribute("data-arrival")).toBe("added");
    expect(within(arrived).getByText("new")).toBeTruthy();
    expect(within(itemOf("September update")).queryByText("new")).toBeNull();
  });

  it("announces nothing the browser already had when a question changes what is shown", async () => {
    const draft = campaignRow({ id: "c1", name: "August update" });
    const sending = campaignRow({ id: "c2", name: "July update", status: "sending", recipientCount: 10, sentCount: 1 });
    const conn = fakeConnection({ campaigns: [draft, sending] });
    mount(conn);
    await screen.findByText("August update");

    fireEvent.click(screen.getByRole("button", { name: "Refine campaigns" }));
    fireEvent.change(screen.getByLabelText("Search"), { target: { value: "july" } });
    // The send ticks while only it is shown...
    await act(async () => { conn.subscriptions.emit(CAMPAIGN, { ...sending, sentCount: 2 }); });
    // ...then the question goes, and the draft is back: it was always here.
    fireEvent.change(screen.getByLabelText("Search"), { target: { value: "" } });
    await act(async () => { conn.subscriptions.emit(CAMPAIGN, { ...sending, sentCount: 3 }); });
    expect(itemOf("August update").getAttribute("data-arrival")).toBeNull();
    expect(within(itemOf("August update")).queryByText("new")).toBeNull();
  });

  it("says when nothing matches", async () => {
    const empty = mount(fakeConnection());
    expect(await screen.findByText("No campaigns yet")).toBeTruthy();
    expect(screen.getByText("Add a campaign to send a template to an audience.")).toBeTruthy();
    expect(headCount(empty.view)).toBe("0");
    // Nothing to ask a question of: no Refine over an empty list (rule 2).
    expect(screen.queryByRole("button", { name: "Refine campaigns" })).toBeNull();
    expect(screen.getByRole("button", { name: "Add a campaign" })).toBeTruthy();
    empty.view.unmount();

    const conn = fakeConnection({ campaigns: [campaignRow({ id: "c1", name: "August update", status: "sent" })] });
    const { view } = mount(conn);
    await screen.findByText("August update");
    fireEvent.click(screen.getByRole("button", { name: "Refine campaigns" }));
    chooseOption(screen.getByLabelText("Status"), "In progress");
    expect(await screen.findByText("No matching campaigns")).toBeTruthy();
    expect(screen.getByText("Try another name or status.")).toBeTruthy();
    expect(screen.queryByText("No campaigns yet")).toBeNull();
    expect(headCount(view)).toBe("0");
    // The question that emptied it stays, so it can be taken back.
    expect(screen.getByRole("button", { name: "Remove In progress" })).toBeTruthy();
  });

  it("opens a campaign's page from its row, and Add a campaign opens the wizard", async () => {
    const conn = fakeConnection({ audiences: AUDIENCES, campaigns: [campaignRow({ id: "c1", name: "August update" })] });
    mount(conn);
    fireEvent.click(await screen.findByText("August update"));
    expect(await screen.findByRole("heading", { level: 3, name: "August update" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Back to Campaigns" }));

    fireEvent.click(await screen.findByRole("button", { name: "Add a campaign" }));
    expect(await screen.findByRole("heading", { level: 3, name: "Add a campaign" })).toBeTruthy();
  });
});
