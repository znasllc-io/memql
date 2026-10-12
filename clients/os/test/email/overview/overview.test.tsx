import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

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
const { EmailNavigationContext } = await import("../../../src/apps/email/openRequest");
const { OverviewSection } = await import("../../../src/apps/email/overview/OverviewSection");
const { LocalEmailSettingsStore } = await import("../../../src/apps/email/settings");
const { EMAIL_REFRESH_MS, useEmailFeeds } = await import("../../../src/apps/email/useEmail");
const {
  audienceRow,
  campaignRow,
  fakeConnection,
  peakSubscriptions,
  rowsResult,
  ruleRow,
  withSession,
} = await import("../harness");

import type { EmailFeeds } from "../../../src/apps/email/useEmail";
import type { FakeSeed } from "../harness";

type Conn = ReturnType<typeof fakeConnection>;

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

// THE OVERVIEW AS A PERSON MEETS IT (memql#6033, Ruling 22).
//
// Five measured figures and the shape of the campaigns, drawn from reads the
// app already makes. What these tests keep honest is what the page does with a
// read that has not answered (a shape, never a warning), one that was refused
// (the engine's sentence, never a zero) and one that found nothing (a real
// zero, and an offer to start).

const DAY = 86_400_000;
const daysAgo = (days: number) => new Date(Date.now() - days * DAY).toISOString();
const DASH = "—";

function memoryStore() {
  const bag = new Map<string, string>();
  return new LocalEmailSettingsStore({
    getItem: (k: string) => bag.get(k) ?? null,
    setItem: (k: string, v: string) => void bag.set(k, v),
  });
}

/** The Email window as a shell holds it: `navigate` moves it, so what a press on the Overview opens can be looked at. */
function mountWindow(connection: Conn, section = "overview") {
  h.connection = connection;
  const navigate = vi.fn();
  const store = memoryStore();
  function Window() {
    const [current, setCurrent] = useState(section);
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
    );
  }
  const view = render(<Window />);
  return { view, navigate };
}

function pane(id: string): HTMLElement {
  const node = document.querySelector<HTMLElement>(`[data-email-section="${id}"]`);
  if (node === null) throw new Error(`the ${id} section is not mounted`);
  return node;
}

/** One figure's cell: its label, its value and what is said beneath. */
function cell(label: string) {
  const node = pane("overview").querySelector<HTMLElement>(`[data-overview-metric="${label}"]`);
  if (node === null) throw new Error(`no figure named ${label}`);
  return {
    value: node.querySelector("dd")?.textContent ?? "",
    note: node.querySelector("small")?.textContent ?? null,
    shaped: node.querySelector("dd [role='status']") !== null,
  };
}

/** `campaignStats` answering per campaign, as the engine does. */
function statsAre(conn: Conn, byCampaign: Record<string, Row>) {
  conn.query.campaignStats.mockImplementation(async (args) => {
    const row = byCampaign[(args as { campaignId?: string }).campaignId ?? ""];
    return rowsResult(row === undefined ? [] : [row]);
  });
}

/** A cluster with a month of mail on it. */
function seeded(over: FakeSeed = {}) {
  const conn = fakeConnection({
    campaigns: [
      campaignRow({ id: "a", name: "July update", status: "sent", startedAt: daysAgo(3), recipientCount: 1000, sentCount: 1000 }),
      campaignRow({ id: "b", name: "August update", status: "sending", startedAt: daysAgo(0), recipientCount: 600, sentCount: 500 }),
      campaignRow({ id: "old", name: "Spring sale", status: "sent", startedAt: daysAgo(60), recipientCount: 9000, sentCount: 9000 }),
      campaignRow({ id: "draft", name: "Autumn sale" }),
    ],
    audiences: [
      audienceRow({ id: "a1", name: "Newsletter" }),
      audienceRow({ id: "a2", name: "Signups" }),
      audienceRow({ id: "a3", name: "Old list", status: "archived" }),
    ],
    subscriberCounts: { a1: 1204, a2: 86, a3: 500 },
    emailRules: [
      ruleRow({ id: "r1", name: "Welcome admins", status: "active" }),
      ruleRow({ id: "r2", name: "Tell the owner", status: "active" }),
      ruleRow({ id: "r3", name: "Old rule", status: "paused" }),
    ],
    ...over,
  });
  // A caller that seeds the breakdowns itself (or a refusal of them) is not overridden.
  if (over.campaignStats === undefined) {
    statsAre(conn, {
      a: { sent: 1000, opens: { unique: 300, total: 420 }, clicks: { unique: 50, total: 60 } },
      b: { sent: 500, opens: { unique: 100, total: 130 }, clicks: { unique: 25, total: 30 } },
    });
  }
  return conn;
}

/** Hold the three lists back until the test lets them go. */
function gated(conn: Conn) {
  let release!: () => void;
  const gate = new Promise<void>((done) => { release = done; });
  for (const read of [conn.query.campaigns, conn.query.audiences, conn.query.emailRules]) {
    const answer = read.getMockImplementation()!;
    read.mockImplementation(async (args, opts) => { await gate; return answer(args, opts); });
  }
  return release;
}

describe("what the Overview measures", () => {
  it("measures what was sent and who is subscribed", async () => {
    mountWindow(seeded());

    // 1,000 + 500 sent in the last 30 days, not the 9,000 of two months ago.
    await waitFor(() => expect(cell("Sent, 30 days").value).toBe("1,500"));
    // (300 + 100) of 1,500 opened; (50 + 25) of 1,500 clicked.
    await waitFor(() => expect(cell("Open rate").value).toBe("27%"));
    expect(cell("Click rate").value).toBe("5.0%");
    // The archived audience is not counted.
    await waitFor(() => expect(cell("Subscribers").value).toBe("1,290"));
    // Two rules are On, one is paused.
    expect(cell("Active automations").value).toBe("2");
    expect(screen.getByRole("heading", { level: 3, name: "Overview" })).toBeTruthy();
    // The page measures; it no longer instructs. The old setup panel is gone.
    expect(screen.queryByText("Sending preparation")).toBeNull();
    expect(screen.queryByRole("button", { name: "Review sending setup" })).toBeNull();
  });

  it("reads a breakdown only for a campaign that can have a rate", async () => {
    const conn = seeded();
    // Counting nothing, sending nothing, and a month old: none of them can.
    conn.query.campaigns.mockResolvedValue(rowsResult([
      campaignRow({ id: "a", status: "sent", startedAt: daysAgo(3), sentCount: 10, recipientCount: 10 }),
      campaignRow({ id: "untracked", status: "sent", startedAt: daysAgo(3), sentCount: 10, recipientCount: 10, trackOpens: false, trackClicks: false }),
      campaignRow({ id: "silent", status: "sending", startedAt: daysAgo(0), sentCount: 0, recipientCount: 10 }),
      campaignRow({ id: "old", status: "sent", startedAt: daysAgo(45), sentCount: 10, recipientCount: 10 }),
      campaignRow({ id: "draft" }),
    ]));
    statsAre(conn, { a: { sent: 10, opens: { unique: 3, total: 3 }, clicks: { unique: 1, total: 1 } } });
    mountWindow(conn);

    await waitFor(() => expect(cell("Open rate").value).toBe("30%"));
    const asked = conn.query.campaignStats.mock.calls.map(([args]) => (args as { campaignId: string }).campaignId);
    expect(asked).toEqual(["a"]);
  });

  it("counts each active audience's subscribers once, and nothing for a cluster with none", async () => {
    const conn = seeded();
    mountWindow(conn);
    await waitFor(() => expect(cell("Subscribers").value).toBe("1,290"));
    const counted = conn.query.audienceSubscriberCount.mock.calls.map(([args]) => (args as { audienceId: string }).audienceId).sort();
    expect(counted).toEqual(["a1", "a2"]);
  });

  it("draws the campaign distribution", async () => {
    const campaigns = [
      campaignRow({ id: "d1", status: "draft" }),
      campaignRow({ id: "d2", status: "draft" }),
      campaignRow({ id: "s1", status: "scheduled", scheduledAt: "2031-10-14T16:00:00Z" }),
      campaignRow({ id: "g1", status: "sending", startedAt: daysAgo(0), recipientCount: 10, sentCount: 4 }),
      campaignRow({ id: "x1", status: "sent", startedAt: daysAgo(2), sentCount: 5, recipientCount: 5 }),
      campaignRow({ id: "x2", status: "sent", startedAt: daysAgo(40), sentCount: 5, recipientCount: 5 }),
      campaignRow({ id: "x3", status: "sent", startedAt: daysAgo(50), sentCount: 5, recipientCount: 5 }),
      campaignRow({ id: "p1", status: "paused", startedAt: daysAgo(1), sentCount: 1, recipientCount: 5 }),
      campaignRow({ id: "f1", status: "failed" }),
    ];
    mountWindow(seeded({ campaigns }));

    const breakdown = await screen.findByRole("region", { name: "Campaigns" });
    // Every campaign the cluster has, not the last 30 days'.
    expect(within(breakdown).getByRole("heading", { level: 4, name: "Campaigns" })).toBeTruthy();
    expect(within(breakdown).getAllByRole("listitem").map((item) => item.textContent)).toEqual([
      "Draft2",
      "Scheduled1",
      "Sending1",
      "Sent3",
      "Other2",
    ]);
  });
});

describe("a read that has not answered", () => {
  it("draws shapes while reading, not a warning", async () => {
    const conn = seeded();
    const release = gated(conn);
    mountWindow(conn);

    // Every figure is the shape of a figure.
    await waitFor(() => expect(cell("Sent, 30 days").shaped).toBe(true));
    for (const label of ["Sent, 30 days", "Open rate", "Click rate", "Subscribers", "Active automations"]) {
      expect(cell(label).shaped).toBe(true);
      expect(cell(label).value).not.toContain(DASH);
    }
    expect(within(pane("overview")).getAllByRole("status").map((node) => node.textContent)).toEqual(
      expect.arrayContaining(["Loading sent, 30 days", "Loading open rate", "Loading click rate", "Loading subscribers", "Loading active automations"]),
    );
    // No warning about nothing being wrong, and no offer to start while the answer is unknown.
    expect(pane("overview").querySelector(".os-notice")).toBeNull();
    expect(screen.queryByText(/not current/i)).toBeNull();
    expect(screen.queryByText("Nothing sent yet")).toBeNull();
    expect(screen.queryByRole("region", { name: "Campaigns" })).toBeNull();

    await act(async () => release());
    await waitFor(() => expect(cell("Sent, 30 days").value).toBe("1,500"));
    expect(cell("Sent, 30 days").shaped).toBe(false);
  });

  it("shows a rate's shape until every breakdown it is made of has answered", async () => {
    const conn = seeded();
    let release!: () => void;
    const gate = new Promise<void>((done) => { release = done; });
    const answer = conn.query.campaignStats.getMockImplementation()!;
    conn.query.campaignStats.mockImplementation(async (args, opts) => { await gate; return answer(args, opts); });
    mountWindow(conn);

    // What does not depend on a breakdown is already there.
    await waitFor(() => expect(cell("Sent, 30 days").value).toBe("1,500"));
    expect(cell("Open rate").shaped).toBe(true);
    expect(cell("Click rate").shaped).toBe(true);
    expect(pane("overview").querySelector(".os-notice")).toBeNull();

    await act(async () => release());
    await waitFor(() => expect(cell("Open rate").value).toBe("27%"));
    expect(cell("Open rate").shaped).toBe(false);
  });
});

describe("a read that was refused", () => {
  it("says what the engine said, never a zero", async () => {
    const conn = seeded({
      campaignStats: new Error("Reading the breakdown is not yours to do."),
      subscriberCounts: { a1: new Error("Counting subscribers is not yours to do."), a2: 86 },
    });
    conn.query.emailRules.mockRejectedValue(new Error("Reading automations is not yours to do."));
    mountWindow(conn);

    await waitFor(() => expect(cell("Open rate").value).toBe(DASH));
    expect(cell("Click rate").value).toBe(DASH);
    expect(cell("Subscribers").value).toBe(DASH);
    expect(cell("Active automations").value).toBe(DASH);
    // What did answer is still a number.
    expect(cell("Sent, 30 days").value).toBe("1,500");

    // Each refusal once, in the engine's own words.
    const text = pane("overview").textContent ?? "";
    for (const sentence of [
      "Reading the breakdown is not yours to do.",
      "Counting subscribers is not yours to do.",
      "Reading automations is not yours to do.",
    ]) {
      expect(text.split(sentence).length - 1).toBe(1);
    }
    expect(text).not.toMatch(/(^|[^\d.])0%/);
  });

  it("is not a first use: a cluster that could not say what it has is not told it has nothing", async () => {
    const conn = fakeConnection();
    conn.query.campaigns.mockRejectedValue(new Error("Campaigns are not yours to read."));
    mountWindow(conn);

    await waitFor(() => expect(cell("Sent, 30 days").value).toBe(DASH));
    expect(screen.getByText("Campaigns are not yours to read.")).toBeTruthy();
    expect(screen.queryByText("Nothing sent yet")).toBeNull();
    // Nor a zero where no number could be had.
    expect(cell("Sent, 30 days").value).not.toBe("0");
  });

  it("leaves a rate it cannot state absent, with the reason beneath it", async () => {
    const conn = seeded();
    statsAre(conn, {
      // The distinct openers were too many to count exactly.
      a: { sent: 1000, opens: { total: 420 }, clicks: { unique: 50, total: 60 } },
      b: { sent: 500, opens: { unique: 100, total: 130 }, clicks: { unique: 25, total: 30 } },
    });
    mountWindow(conn);

    await waitFor(() => expect(cell("Click rate").value).toBe("5.0%"));
    expect(cell("Open rate").value).toBe(DASH);
    expect(cell("Open rate").note).toBe("Too many recorded events to count distinct recipients exactly, so this is left unstated rather than guessed.");
    // A reason is not a failure: nothing was refused.
    expect(pane("overview").querySelector(".os-notice")).toBeNull();
  });

  it("says why there is no rate when nothing counted opens or clicks", async () => {
    const conn = seeded({
      campaigns: [campaignRow({ id: "u", status: "sent", startedAt: daysAgo(2), sentCount: 40, recipientCount: 40, trackOpens: false, trackClicks: false })],
    });
    mountWindow(conn);

    await waitFor(() => expect(cell("Sent, 30 days").value).toBe("40"));
    expect(cell("Open rate").value).toBe(DASH);
    expect(cell("Open rate").note).toBe("No tracked campaign in the last 30 days.");
    expect(cell("Click rate").note).toBe("No tracked campaign in the last 30 days.");
    expect(conn.query.campaignStats).not.toHaveBeenCalled();
  });
});

describe("first use", () => {
  it("first use offers Add a campaign", async () => {
    const { navigate } = mountWindow(fakeConnection());

    expect(await screen.findByRole("heading", { level: 4, name: "Nothing sent yet" })).toBeTruthy();
    expect(screen.getByText("Add a campaign to send a template to an audience.")).toBeTruthy();
    // Nothing has been sent and nobody is subscribed: real zeros, said by the page's one sentence.
    await waitFor(() => expect(cell("Sent, 30 days").value).toBe("0"));
    expect(cell("Subscribers").value).toBe("0");
    expect(cell("Active automations").value).toBe("0");
    expect(screen.queryByRole("region", { name: "Campaigns" })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Add a campaign" }));

    // The Campaigns tab, on its own wizard.
    expect(navigate).toHaveBeenCalledWith("campaigns", { fromContent: true });
    expect(await screen.findByRole("heading", { level: 3, name: "Add a campaign" })).toBeTruthy();
    expect(pane("overview").hidden).toBe(true);
  });

  it("is not offered once there is a campaign, or an automation, to speak of", async () => {
    const withCampaign = mountWindow(fakeConnection({ campaigns: [campaignRow({ id: "d" })] }));
    await waitFor(() => expect(screen.getByRole("region", { name: "Campaigns" })).toBeTruthy());
    expect(screen.queryByText("Nothing sent yet")).toBeNull();
    withCampaign.view.unmount();

    // A paused automation is still an automation.
    mountWindow(fakeConnection({ emailRules: [ruleRow({ id: "r", status: "paused" })] }));
    await waitFor(() => expect(cell("Sent, 30 days").value).toBe("0"));
    expect(screen.queryByText("Nothing sent yet")).toBeNull();
  });
});

describe("when updates stop", () => {
  type Stoppable = { markDisconnected: () => void };

  function standalone(conn: Conn) {
    h.connection = conn;
    const held: { feeds: EmailFeeds | null } = { feeds: null };
    const navigation = { openRecord: vi.fn(), openSettings: vi.fn(), openInbox: vi.fn() };
    function Page() {
      const feeds = useEmailFeeds();
      held.feeds = feeds;
      return (
        <EmailNavigationContext.Provider value={navigation}>
          <OverviewSection feeds={feeds} />
        </EmailNavigationContext.Provider>
      );
    }
    render(<Page />);
    return held;
  }

  it("keeps the last figures and says updates are interrupted", async () => {
    const held = standalone(seeded());
    await waitFor(() => expect(document.querySelector("[data-overview-metric='Sent, 30 days'] dd")?.textContent).toBe("1,500"));
    expect(document.querySelector(".os-notice")).toBeNull();

    act(() => (held.feeds!.campaigns.source as unknown as Stoppable).markDisconnected());

    expect(await screen.findByText("Updates are interrupted.")).toBeTruthy();
    expect(screen.getByText("Showing the last figures received. Reconnecting automatically.")).toBeTruthy();
    expect(document.querySelector("[data-overview-metric='Sent, 30 days'] dd")?.textContent).toBe("1,500");
  });

  it("says it is reconnecting, and claims no figures, when nothing had been received", async () => {
    const conn = seeded();
    gated(conn);
    const held = standalone(conn);
    await act(async () => {});

    act(() => (held.feeds!.campaigns.source as unknown as Stoppable).markDisconnected());

    expect(await screen.findByText("Updates are interrupted.")).toBeTruthy();
    expect(screen.getByText("Reconnecting automatically.")).toBeTruthy();
    expect(screen.queryByText(/Showing the last figures/)).toBeNull();
    // Nothing has been counted, so nothing is claimed: no zero, no first-use offer.
    expect(document.querySelector("[data-overview-metric='Sent, 30 days'] dd")?.textContent).not.toBe("0");
    expect(screen.queryByText("Nothing sent yet")).toBeNull();
  });
});

describe("how the Overview reads", () => {
  it("adds no subscription of its own", async () => {
    const conn = seeded();
    mountWindow(conn);
    await waitFor(() => expect(cell("Open rate").value).toBe("27%"));
    for (const [concept, peak] of peakSubscriptions(conn)) expect(peak, concept).toBeLessThanOrEqual(1);
  });

  it("reads again every five seconds while it is seen, and stops when another tab is", async () => {
    const conn = seeded();
    h.connection = conn;
    const store = memoryStore();
    const app = (section: string) =>
      withSession(<EmailApp sectionId={section} navigate={() => {}} askContext={() => {}} store={store} uploads={{ upload: vi.fn() }} />);
    const view = render(app("overview"));
    await waitFor(() => expect(cell("Open rate").value).toBe("27%"));
    await waitFor(() => expect(cell("Subscribers").value).toBe("1,290"));
    await act(async () => {});

    const reads = () => conn.query.campaignStats.mock.calls.length + conn.query.audienceSubscriberCount.mock.calls.length;
    vi.useFakeTimers();
    // Coming back to the window arms the refresher on the clock this test holds.
    await act(async () => window.dispatchEvent(new Event("focus")));
    const armed = reads();
    await act(async () => { await vi.advanceTimersByTimeAsync(EMAIL_REFRESH_MS); });
    expect(reads()).toBeGreaterThan(armed);

    // The person goes to Campaigns. The Overview stays mounted behind it, and stops reading.
    view.rerender(app("campaigns"));
    expect(pane("overview").hidden).toBe(true);
    const left = reads();
    await act(async () => { await vi.advanceTimersByTimeAsync(EMAIL_REFRESH_MS * 3); });
    expect(reads()).toBe(left);
  });
});
