import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";

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
const { audienceRow, campaignRow, fakeConnection, senderRow, templateRow, withSession } = await import("../harness");

// A CAMPAIGN THAT REPEATS (memql#6033, Ruling 7c).
//
// A draft a series sends from reads as the series does -- Repeating, Repeating
// paused, Repeating stopped -- and its bar pauses and resumes the series under
// the revision it was read at. The series is one row of the app's one series
// feed: a change re-reads that campaign's series and nobody else's.

type Conn = ReturnType<typeof fakeConnection>;

const SERIES = "v1:campaigns:campaignSeries";
const CAMPAIGN = "v1:campaigns:campaign";
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

afterEach(() => localStorage.clear());

const series = {
  id: "series",
  sourceCampaignId: "c1",
  accountId: "self",
  name: "Weekly digest",
  status: "active",
  intervalWeeks: 2,
  timeZone: ZONE,
  anchorAt: OCT_14,
  nextAt: OCT_14,
  createdAt: "2027-10-01T12:00:00.000001Z",
  lastError: "",
};

function seed(over: Parameters<typeof fakeConnection>[0] = {}) {
  return fakeConnection({
    audiences: [audienceRow({ id: "a1", name: "Newsletter" })],
    templates: [templateRow({ id: "t1", name: "Digest" })],
    senderIdentities: [senderRow({ id: "s1", address: "news@acme.com" })],
    subscriberCounts: { a1: 1204 },
    campaigns: [
      campaignRow({ id: "c1", name: "Weekly digest", audienceId: "a1", templateId: "t1", senderIdentityId: "s1" }),
      campaignRow({ id: "c2", name: "Other digest", audienceId: "a1", templateId: "t1", senderIdentityId: "s1" }),
    ],
    campaignSeries: [series],
    ...over,
  });
}

function mountWindow(connection: Conn) {
  h.connection = connection;
  const bag = new Map<string, string>();
  const store = new LocalEmailSettingsStore({ getItem: (k) => bag.get(k) ?? null, setItem: (k, v) => void bag.set(k, v) });
  function Window() {
    const [current, setCurrent] = useState("campaigns");
    return withSession(<EmailApp sectionId={current} navigate={(id) => setCurrent(id)} askContext={() => {}} store={store} uploads={{ upload: vi.fn() }} />);
  }
  return render(<Window />);
}

const bar = () => screen.getByRole("group", { name: "What you can do with this" });
const barWord = () => bar().querySelector(".os-actbar-word")?.textContent ?? "";
const barDetail = () => bar().querySelector(".os-actbar-detail")?.textContent ?? "";
const barActs = () => within(bar()).queryAllByRole("button").map((button) => button.textContent);
const press = (name: string) => fireEvent.click(within(bar()).getByRole("button", { name }));

async function openCampaign(name = "Weekly digest") {
  fireEvent.click(await screen.findByText(name));
  expect(await screen.findByRole("heading", { level: 3, name })).toBeTruthy();
}

/** The campaigns whose series were read since the count was taken. */
const seriesReadsFor = (conn: Conn, from: number) => conn.query.campaignSeriesForCampaign.mock.calls.slice(from).map(([args]) => (args as { campaignId: string }).campaignId);

describe("a campaign that repeats", () => {
  it("a repeating campaign reads its series, pauses with its revision and resumes", async () => {
    const conn = seed();
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barWord()).toBe("Repeating"));
    expect(barDetail()).toBe("Every 2 weeks · next Tue, Oct 14 at 9:00 (America/Phoenix)");
    expect(barActs()).toEqual(["Edit", "Send a test", "Pause"]);

    const before = conn.query.campaignSeriesForCampaign.mock.calls.length;
    press("Pause");
    await waitFor(() => expect(conn.query.campaignConfigureSeries).toHaveBeenCalledWith({ campaignId: "c1", action: "pause", expectedRevision: series.createdAt }));
    // The one series that changed is read again; the other campaign's is not.
    await waitFor(() => expect(seriesReadsFor(conn, before)).toContain("c1"));
    expect(seriesReadsFor(conn, before)).not.toContain("c2");

    await act(async () => conn.subscriptions.emit(SERIES, { ...series, status: "paused", createdAt: "2027-10-01T12:01:00Z" }));
    await waitFor(() => expect(barWord()).toBe("Repeating paused"));
    expect(barActs()).toEqual(["Edit", "Send a test", "Resume"]);
    press("Resume");
    await waitFor(() => expect(conn.query.campaignConfigureSeries).toHaveBeenLastCalledWith({ campaignId: "c1", action: "resume", expectedRevision: "2027-10-01T12:01:00Z" }));
  });

  it("ignores another campaign's series event", async () => {
    const conn = seed();
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barDetail()).toBe("Every 2 weeks · next Tue, Oct 14 at 9:00 (America/Phoenix)"));
    await act(async () => conn.subscriptions.emit(SERIES, { ...series, id: "other", sourceCampaignId: "c2", intervalWeeks: 40 }));
    expect(barDetail()).toBe("Every 2 weeks · next Tue, Oct 14 at 9:00 (America/Phoenix)");
    expect(screen.queryByText(/Every 40 weeks/)).toBeNull();
  });

  it("a stopped series shows its reason verbatim and offers Resume", async () => {
    const conn = seed({ campaignSeries: [{ ...series, status: "blocked", lastError: "Publish the template before resuming this schedule." }] });
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barWord()).toBe("Repeating stopped"));
    expect(barDetail()).toBe("Publish the template before resuming this schedule.");
    expect(barActs()).toEqual(["Edit", "Resume"]);
    press("Resume");
    await waitFor(() => expect(conn.query.campaignConfigureSeries).toHaveBeenCalledWith({ campaignId: "c1", action: "resume", expectedRevision: series.createdAt }));
  });

  it("a refused series change is said in the engine's words", async () => {
    const conn = seed();
    conn.query.campaignConfigureSeries.mockRejectedValueOnce(new Error("this recurring campaign changed; reload it before saving"));
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toContain("Pause"));
    press("Pause");
    expect(await screen.findByText("Repeating wasn't paused.")).toBeTruthy();
    expect(screen.getByText("this recurring campaign changed; reload it before saving")).toBeTruthy();
  });

  it("a repeating campaign that was sent still pauses its series", async () => {
    // A draft a series repeats from was then sent by hand: the series still
    // runs, and the page that is the only way to it keeps Pause within reach.
    const conn = seed({ campaigns: [campaignRow({ id: "c1", name: "Weekly digest", audienceId: "a1", templateId: "t1", senderIdentityId: "s1", status: "sent", recipientCount: 10, sentCount: 10 })] });
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toEqual(["Send a test", "Duplicate", "Pause"]));
    expect(barWord()).toBe("Sent");
    press("Pause");
    await waitFor(() => expect(conn.query.campaignConfigureSeries).toHaveBeenCalledWith({ campaignId: "c1", action: "pause", expectedRevision: series.createdAt }));
  });

  it("saving a repeating campaign's edits saves its series with its revision", async () => {
    // The first send as the engine wrote it, to the millisecond: left alone, it
    // goes back exactly so, and the engine knows the cadence did not move.
    const conn = seed({ campaignSeries: [{ ...series, anchorAt: "2031-10-14T16:00:00.000Z" }] });
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toContain("Edit"));
    press("Edit");
    expect(await screen.findByRole("heading", { level: 3, name: "Edit campaign" })).toBeTruthy();
    // A repeat's own fields: how often, and from when.
    expect(screen.queryByLabelText("When")).toBeNull();
    expect((screen.getByLabelText("Starting · America/Phoenix") as HTMLInputElement).value).toBe("2031-10-14T09:00");
    chooseOption(screen.getByLabelText("Every"), "3 weeks");
    press("Save");

    await waitFor(() =>
      expect(conn.query.campaignConfigureSeries).toHaveBeenCalledWith({
        campaignId: "c1",
        action: "save",
        intervalWeeks: 3,
        firstSendAt: "2031-10-14T16:00:00.000Z",
        timeZone: ZONE,
        expectedRevision: series.createdAt,
      }),
    );
    // How often it repeats is the series', not the campaign's: nothing else was written.
    expect(conn.query.updateCampaign).not.toHaveBeenCalled();
    expect(await screen.findByRole("heading", { level: 3, name: "Weekly digest" })).toBeTruthy();
  });

  it("a campaign edit saves the series too, so it repeats what was saved; a stale revision keeps the edits", async () => {
    const conn = seed();
    conn.query.campaignConfigureSeries.mockRejectedValueOnce(new Error("this recurring campaign changed; reload it before saving"));
    mountWindow(conn);
    await openCampaign();
    await waitFor(() => expect(barActs()).toContain("Edit"));
    press("Edit");
    fireEvent.change(await screen.findByLabelText("Name"), { target: { value: "Fortnightly digest" } });
    press("Save");

    await waitFor(() => expect(conn.query.updateCampaign).toHaveBeenCalledOnce());
    expect(conn.query.updateCampaign.mock.calls[0]![0]).toEqual({ campaignId: "c1", name: "Fortnightly digest", audienceId: "a1", templateId: "t1" });
    await waitFor(() => expect(conn.query.campaignConfigureSeries).toHaveBeenCalledWith(expect.objectContaining({ campaignId: "c1", action: "save", intervalWeeks: 2, expectedRevision: series.createdAt })));
    expect(await screen.findByText("The campaign was saved, but its repeating schedule wasn't.")).toBeTruthy();
    expect(screen.getByText("this recurring campaign changed; reload it before saving")).toBeTruthy();
    expect((screen.getByLabelText("Name") as HTMLInputElement).value).toBe("Fortnightly digest");

    // A newer series arrives; trying again saves under its revision.
    await act(async () => conn.subscriptions.emit(SERIES, { ...series, createdAt: "2027-10-02T09:00:00Z" }));
    await act(async () => conn.subscriptions.emit(CAMPAIGN, campaignRow({ id: "c1", name: "Fortnightly digest", audienceId: "a1", templateId: "t1", senderIdentityId: "s1" })));
    press("Save");
    await waitFor(() => expect(conn.query.campaignConfigureSeries).toHaveBeenLastCalledWith(expect.objectContaining({ expectedRevision: "2027-10-02T09:00:00Z" })));
  });
});
