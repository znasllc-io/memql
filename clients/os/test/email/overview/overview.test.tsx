import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { chooseOption, openSelect, selectedLabel } from "../../selectControl";

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
const { OVERVIEW_REFRESH_MS, OverviewSection } = await import("../../../src/apps/email/overview/OverviewSection");
const { STATISTICS_REFRESH_MS } = await import("../../../src/apps/email/overview/statistics");
const { DEFAULT_EMAIL_SETTINGS, LocalEmailSettingsStore } = await import("../../../src/apps/email/settings");
const { useEmailFeeds } = await import("../../../src/apps/email/useEmail");
const {
  audienceRow,
  campaignRow,
  fakeConnection,
  peakSubscriptions,
  rowsResult,
  ruleRow,
  statisticsRow,
  twelveMonthsStatisticsRow,
  unmeasuredStatisticsRow,
  withSession,
} = await import("../harness");

import type { EmailSettings } from "../../../src/apps/email/settings";
import type { EmailFeeds } from "../../../src/apps/email/useEmail";
import type { FakeSeed } from "../harness";

type Conn = ReturnType<typeof fakeConnection>;
type Session = Parameters<typeof withSession>[1];

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

// THE OVERVIEW AS A PERSON MEETS IT (spec 9): the period's email statistics for
// one client or every client in scope, read from `emailStatistics`.
//
// Five figures and the chart of the one pressed; what is true now (who can be
// mailed, what runs on its own); delivery problems with their own figures and
// chart; how each audience grew; the campaigns most opened; each automation's
// mail; the campaigns by state; and, over several clients, the clients. What
// these tests keep honest is what the page does with a read that has not
// answered (a shape, never a word), one that was refused (the engine's
// sentence, never a zero) and a period nothing counted (a dash that says so,
// never a zero) -- and that a person sees only the clients they reach.

const DASH = "—";

const ACCOUNTS = [
  { id: "self", name: "Operator organization", status: "active" },
  { id: "acct-a", name: "Acme", status: "active" },
  { id: "acct-b", name: "Borden Ltd", status: "active" },
];

/** The read's organizations when it covers two: by sent, the second with no campaign mail to rate. */
const TWO_CLIENTS = [
  { id: "acct-a", sent: 900, uniqueOpens: 400, uniqueClicks: 30, openRate: 0.45, clickRate: 0.033 },
  { id: "acct-b", sent: 304, uniqueOpens: 0, uniqueClicks: 0, openRate: null, clickRate: null },
];

function journeyRow(over: Partial<Row> & { id: string }): Row {
  return { accountId: "self", name: "Welcome series", description: "", status: "draft", trigger: { kind: "joined", audienceId: "a-news" }, steps: [], createdAt: "2026-08-01T00:00:00Z", ...over };
}

function memoryStore(over: Partial<EmailSettings> = {}) {
  const bag = new Map<string, string>();
  const store = new LocalEmailSettingsStore({
    getItem: (k: string) => bag.get(k) ?? null,
    setItem: (k: string, v: string) => void bag.set(k, v),
  });
  if (Object.keys(over).length > 0) store.save({ ...store.load(), ...over });
  return store;
}

/** The Email window as a shell holds it: `navigate` moves it, so what a press on the Overview opens can be looked at. */
function mountWindow(connection: Conn, { section = "overview", settings = {}, session = {} }: { section?: string; settings?: Partial<EmailSettings>; session?: Session } = {}) {
  h.connection = connection;
  const navigate = vi.fn();
  const store = memoryStore(settings);
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
      session,
    );
  }
  const view = render(<Window />);
  return { view, navigate, store };
}

/** The Overview on its own over the root's feeds, with the feeds in hand: what a test needs to make one of them fail. */
function standalone(conn: Conn) {
  h.connection = conn;
  const held: { feeds: EmailFeeds | null } = { feeds: null };
  const navigation = { openRecord: vi.fn(), openSettings: vi.fn(), openInbox: vi.fn() };
  function Page() {
    const feeds = useEmailFeeds();
    const [settings, setSettings] = useState<EmailSettings>(DEFAULT_EMAIL_SETTINGS);
    held.feeds = feeds;
    return (
      <EmailNavigationContext.Provider value={navigation}>
        <OverviewSection feeds={feeds} settings={settings} update={(patch) => setSettings((now) => ({ ...now, ...patch }))} />
      </EmailNavigationContext.Provider>
    );
  }
  render(withSession(<Page />));
  return held;
}

function pane(id: string): HTMLElement {
  const node = document.querySelector<HTMLElement>(`[data-email-section="${id}"]`);
  if (node === null) throw new Error(`the ${id} section is not mounted`);
  return node;
}

/** One figure of a figure row: what it says, and whether it is a shape, or the one its chart draws. */
function figure(id: string) {
  const node = document.querySelector<HTMLElement>(`[data-stat-figure="${id}"]`);
  if (node === null) throw new Error(`no figure ${id}`);
  return {
    node,
    label: node.querySelector(".os-stat-figure-label")?.textContent ?? "",
    value: node.querySelector(".os-stat-figure-value")?.textContent ?? "",
    change: node.querySelector(".os-stat-figure-change")?.textContent ?? "",
    shaped: node.querySelector(".os-stat-figure-value [role='status']") !== null,
    /** What the dash says on hover. */
    hover: node.querySelector(".os-figure-absent")?.getAttribute("title") ?? null,
    checked: node.getAttribute("aria-checked"),
  };
}

/** A chart: what it draws, its one sentence, whether a previous period is drawn under it, and what an empty plot says. */
function chart(node: Element | null) {
  if (node === null) throw new Error("no chart");
  const plot = node.querySelector("[aria-roledescription='chart']");
  return {
    label: plot?.getAttribute("aria-label") ?? "",
    summary: node.querySelector("figcaption")?.textContent ?? "",
    previous: node.querySelector("[data-series='previous']") !== null,
    empty: node.querySelector(".os-time-chart-empty")?.textContent ?? "",
  };
}

/** The page's own chart, directly under its figures. */
const mainChart = () => chart(document.querySelector(".os-overview > .os-time-chart"));

/** A fact of the page: its value as read. */
function fact(label: string, within: ParentNode = document): string {
  const row = [...within.querySelectorAll(".os-fact")].find((node) => node.querySelector("dt")?.textContent === label);
  if (row === undefined) throw new Error(`no fact ${label}`);
  return row.querySelector("dd")?.textContent ?? "";
}

/** A record list's rows as read: the name, then its quiet line. */
function records(section: HTMLElement): string[] {
  return [...section.querySelectorAll(".os-record-row")].map((row) =>
    [row.querySelector(".os-row-name")?.textContent ?? "", row.querySelector(".os-record-secondary")?.textContent ?? ""].filter(Boolean).join(" "),
  );
}

/** A ranked list's rows as read: label, quiet line, value. */
function ranked(section: HTMLElement): string[] {
  return [...section.querySelectorAll(".os-ranked-row")].map((row) =>
    [row.querySelector(".os-ranked-label")?.textContent, row.querySelector(".os-ranked-secondary")?.textContent, row.querySelector(".os-ranked-value")?.textContent].filter(Boolean).join(" | "),
  );
}

/** The notices on the page: how many stories it tells. */
const notices = () => [...document.querySelectorAll<HTMLElement>(".os-notice")].map((node) => node.textContent ?? "");

/** What the heading's line says has stopped arriving. */
const headNote = () => document.querySelector<HTMLElement>(".os-overview > .os-head .os-head-note");

/** A cluster with a month of mail on it. */
function seeded(over: FakeSeed = {}) {
  return fakeConnection({
    accounts: ACCOUNTS,
    campaigns: [
      campaignRow({ id: "c-winter", name: "Winter launch", status: "sent", startedAt: "2026-10-20T09:00:00Z", recipientCount: 600, sentCount: 600 }),
      campaignRow({ id: "c-restock", name: "Restock alert", status: "sending", startedAt: "2026-11-07T09:00:00Z", recipientCount: 700, sentCount: 604 }),
      campaignRow({ id: "c-draft", name: "Autumn sale" }),
      campaignRow({ id: "c-borden", name: "Borden spring", accountId: "acct-b" }),
    ],
    audiences: [
      audienceRow({ id: "a-news", name: "Newsletter" }),
      audienceRow({ id: "a-signups", name: "Signups" }),
      audienceRow({ id: "a-old", name: "Old list", status: "archived" }),
      audienceRow({ id: "a-borden", name: "Borden customers", accountId: "acct-b" }),
    ],
    subscriberCounts: { "a-news": 1204, "a-signups": 86, "a-old": 500, "a-borden": 40 },
    emailRules: [
      ruleRow({ id: "r-welcome", name: "Welcome admins", status: "active" }),
      ruleRow({ id: "r-paused", name: "Old rule", status: "paused" }),
      ruleRow({ id: "r-borden", name: "Borden receipts", status: "active", accountId: "acct-b" }),
    ],
    journeys: [journeyRow({ id: "j-series", name: "Welcome series", status: "active" })],
    emailStatistics: [statisticsRow()],
    ...over,
  });
}

/** Hold the statistics read back until the test lets it go. */
function gatedStatistics(conn: Conn) {
  let release!: () => void;
  const gate = new Promise<void>((done) => { release = done; });
  const answer = conn.query.emailStatistics.getMockImplementation()!;
  conn.query.emailStatistics.mockImplementation(async (args, opts) => { await gate; return answer(args, opts); });
  return release;
}

/** The last arguments the statistics read was called with. */
const lastAsked = (conn: Conn) => conn.query.emailStatistics.mock.calls.at(-1)?.[0];

describe("the period's figures", () => {
  it("shows the period's figures from the read", async () => {
    const conn = seeded();
    mountWindow(conn);

    await waitFor(() => expect(figure("sent").value).toBe("1,204"));
    expect([...document.querySelectorAll(".os-overview > .os-stat-figures [data-stat-figure]")].map((node) => node.querySelector(".os-stat-figure-label")?.textContent)).toEqual([
      "Sent",
      "Open rate",
      "Click rate",
      "Unsubscribes",
      "New subscribers",
    ]);
    // Each against the thirty days before, which were counted throughout.
    expect(figure("sent").change).toBe("+12%");
    expect(figure("openRate")).toMatchObject({ value: "42%", change: "+5%" });
    expect(figure("clickRate")).toMatchObject({ value: "3.1%", change: "No change" });
    expect(figure("unsubscribes")).toMatchObject({ value: "38", change: "−25%" });
    expect(figure("subscribes")).toMatchObject({ value: "212", change: "New" });

    // Read for the last thirty days, for every client in scope: no client named.
    expect(conn.query.emailStatistics).toHaveBeenCalledTimes(1);
    expect(lastAsked(conn)).toEqual({ accountIds: [], period: "30d" });

    // What is true now, beneath: the active audiences' subscriptions and what is switched on.
    await waitFor(() => expect(fact("Subscriptions")).toBe("1,330"));
    expect(fact("Active automations")).toBe("3");
    expect(screen.getByRole("heading", { level: 3, name: "Overview" })).toBeTruthy();
    expect(selectedLabel(screen.getByLabelText("Period"))).toBe("Last 30 days");
  });

  it("states a rate from the campaign mail the read counted, not from the period's opens", async () => {
    // 900 messages were first opened in the period: 75% of the 1,204 sent. The
    // open rate is of the campaign messages sent in it, opened on any day: 42%.
    mountWindow(seeded());

    await waitFor(() => expect(figure("openRate").value).toBe("42%"));
    expect(figure("openRate").value).not.toBe("75%");
    expect(figure("clickRate").value).toBe("3.1%");
  });

  it("charts the figure pressed, against the previous period", async () => {
    mountWindow(seeded());

    await waitFor(() => expect(mainChart().label).toBe("Sent"));
    expect(figure("sent").checked).toBe("true");
    expect(mainChart().summary).toBe("Sent: 30 points from Oct 10 to Nov 8; highest 64 on Oct 15.");
    expect(mainChart().previous).toBe(true);

    fireEvent.click(figure("openRate").node);
    expect(figure("openRate").checked).toBe("true");
    expect(figure("sent").checked).toBe("false");
    expect(mainChart().label).toBe("Open rate");
    expect(mainChart().summary).toBe("Open rate: 30 points from Oct 10 to Nov 8; highest 55% on Oct 30.");

    // The figures are one choice: the arrow keys move it.
    fireEvent.keyDown(figure("openRate").node, { key: "ArrowRight" });
    expect(mainChart().label).toBe("Click rate");
    expect(document.activeElement).toBe(figure("clickRate").node);
  });

  it("draws shapes while it reads, never a word", async () => {
    const conn = seeded();
    const release = gatedStatistics(conn);
    mountWindow(conn);

    await waitFor(() => expect(figure("sent").shaped).toBe(true));
    for (const id of ["sent", "openRate", "clickRate", "unsubscribes", "subscribes", "bounces", "complaints", "failed", "suppressed"]) {
      expect(figure(id).shaped).toBe(true);
      expect(figure(id).value).not.toContain(DASH);
    }
    // The chart's place holds a shape too (kit TimeChartSkeleton): no plot to
    // read or focus yet.
    expect(document.querySelector(".os-time-chart[aria-busy='true']")).not.toBeNull();
    expect(within(pane("overview")).queryByRole("application")).toBeNull();
    expect(within(pane("overview")).getAllByRole("status").map((node) => node.textContent)).toEqual(
      expect.arrayContaining(["Loading sent", "Loading sent over time", "Loading audience growth", "Loading top campaigns", "Loading automations"]),
    );
    expect(pane("overview").querySelector(".os-notice")).toBeNull();
    expect(screen.queryByText("Nothing sent yet")).toBeNull();

    await act(async () => release());
    await waitFor(() => expect(figure("sent").value).toBe("1,204"));
    expect(figure("sent").shaped).toBe(false);
    expect(mainChart().label).toBe("Sent");
  });
});

describe("delivery problems", () => {
  it("charts delivery problems from their own figures", async () => {
    mountWindow(seeded());

    const section = await screen.findByRole("region", { name: "Delivery problems" });
    await waitFor(() => expect(figure("bounces").value).toBe("7"));
    expect(within(section).getByRole("radiogroup").querySelectorAll("[data-stat-figure]").length).toBe(4);
    expect([...within(section).getAllByRole("radio")].map((node) => node.getAttribute("data-stat-figure"))).toEqual(["bounces", "complaints", "failed", "suppressed"]);
    expect(figure("bounces")).toMatchObject({ label: "Bounces", change: "No change" });
    expect(figure("complaints")).toMatchObject({ label: "Complaints", value: "1", change: "New" });
    expect(figure("failed")).toMatchObject({ label: "Failed", value: "3", change: "−50%" });
    expect(figure("suppressed")).toMatchObject({ label: "Suppressed", value: "4", change: "No change" });

    const own = () => chart(section.querySelector(".os-time-chart"));
    expect(own().label).toBe("Bounces");
    expect(own().summary).toBe("Bounces: 30 points from Oct 10 to Nov 8; highest 2 on Oct 13.");

    fireEvent.click(figure("complaints").node);
    expect(own().label).toBe("Complaints");
    // The page's own figure and chart are not moved by it.
    expect(figure("sent").checked).toBe("true");
    expect(mainChart().label).toBe("Sent");
  });

  it("says how many were delivered only where the provider reports outcomes", async () => {
    const reported = mountWindow(seeded({ emailStatistics: [statisticsRow({ totals: { delivered: 1180, undelivered: 9 } })] }));
    const section = await screen.findByRole("region", { name: "Delivery problems" });
    await waitFor(() => expect(fact("Delivered", section)).toBe("1,180"));
    reported.view.unmount();

    // None delivered and four refused is still the provider reporting.
    const refused = mountWindow(seeded({ emailStatistics: [statisticsRow({ totals: { delivered: 0, undelivered: 4 } })] }));
    await waitFor(() => expect(fact("Delivered", screen.getByRole("region", { name: "Delivery problems" }))).toBe("0"));
    refused.view.unmount();

    // No delivered or undelivered fact in the period: the provider reports no outcomes, and Delivered is not said at all.
    mountWindow(seeded());
    await waitFor(() => expect(figure("bounces").value).toBe("7"));
    expect(within(screen.getByRole("region", { name: "Delivery problems" })).queryByText("Delivered")).toBeNull();
    expect([...document.querySelectorAll(".os-fact dt")].map((node) => node.textContent)).not.toContain("Delivered");
  });
});

describe("the period's lists", () => {
  it("lists each audience's growth, and a row opens the audience", async () => {
    const { navigate } = mountWindow(seeded());

    const section = await screen.findByRole("region", { name: "Audience growth" });
    await waitFor(() => expect(records(section)).toEqual([
      "Newsletter 12 new · 3 unsubscribed · +9 net · 1,204 subscribed",
      "Signups 2 new · 5 unsubscribed · −3 net · 86 subscribed",
    ]));

    fireEvent.click(within(section).getByRole("button", { name: /^Newsletter/ }));
    expect(navigate).toHaveBeenCalledWith("audiences", { fromContent: true });
    expect(await screen.findByRole("heading", { level: 3, name: "Newsletter" })).toBeTruthy();
    expect(pane("overview").hidden).toBe(true);
  });

  it("leaves an audience's size out until it has been counted", async () => {
    const conn = seeded();
    let release!: () => void;
    const gate = new Promise<void>((done) => { release = done; });
    const count = conn.query.audienceSubscriberCount.getMockImplementation()!;
    conn.query.audienceSubscriberCount.mockImplementation(async (args, opts) => { await gate; return count(args, opts); });
    mountWindow(conn);

    const section = await screen.findByRole("region", { name: "Audience growth" });
    await waitFor(() => expect(records(section)[0]).toBe("Newsletter 12 new · 3 unsubscribed · +9 net"));
    expect(section.textContent).not.toContain("0 subscribed");

    await act(async () => release());
    await waitFor(() => expect(records(section)[0]).toBe("Newsletter 12 new · 3 unsubscribed · +9 net · 1,204 subscribed"));
  });

  it("ranks the top campaigns by unique opens in the read's order, and a row opens the campaign", async () => {
    const { navigate } = mountWindow(seeded());

    const list = await screen.findByRole("region", { name: "Top campaigns" });
    // The read's order, valued by unique opens: Restock alert sent more and was opened less.
    await waitFor(() => expect(ranked(list)).toEqual([
      "Winter launch | 50% opened · 5.7% clicked | 300",
      "Restock alert | 33% opened · 3.2% clicked | 200",
    ]));

    fireEvent.click(within(list).getByRole("button", { name: /Winter launch/ }));
    expect(navigate).toHaveBeenCalledWith("campaigns", { fromContent: true });
    expect(await screen.findByRole("heading", { level: 3, name: "Winter launch" })).toBeTruthy();
  });

  it("lists each automation's sent, failed and suppressed, and a row opens its page", async () => {
    const { navigate } = mountWindow(seeded());

    const section = await screen.findByRole("region", { name: "Automations" });
    await waitFor(() => expect(records(section)).toEqual([
      "Welcome admins 120 sent · 2 failed · 1 suppressed",
      "Welcome series 40 sent · 0 failed · 0 suppressed",
    ]));

    fireEvent.click(within(section).getByRole("button", { name: /^Welcome admins/ }));
    expect(navigate).toHaveBeenCalledWith("automations", { fromContent: true });
    expect(await screen.findByRole("heading", { level: 3, name: "Welcome admins" })).toBeTruthy();
  });

  it("names a record the lists do not hold as unavailable, and does not offer to open it", async () => {
    mountWindow(seeded({
      emailStatistics: [statisticsRow({
        campaigns: [{ id: "c-gone", sent: 10, uniqueOpens: 4, uniqueClicks: 1, openRate: 0.4, clickRate: 0.1 }],
        audiences: [{ id: "a-gone", subscribes: 1, unsubscribes: 0, net: 1 }],
        automations: [{ id: "r-gone", kind: "rule", sent: 5, failed: 0, suppressed: 0 }],
      })],
    }));

    const campaigns = await screen.findByRole("region", { name: "Top campaigns" });
    await waitFor(() => expect(ranked(campaigns)).toEqual(["Unavailable campaign | 40% opened · 10% clicked | 4"]));
    expect(within(campaigns).queryByRole("button")).toBeNull();
    const growth = screen.getByRole("region", { name: "Audience growth" });
    expect(records(growth)).toEqual(["Unavailable audience 1 new · 0 unsubscribed · +1 net"]);
    expect(within(growth).queryByRole("button")).toBeNull();
    expect(records(screen.getByRole("region", { name: "Automations" }))).toEqual(["Unavailable automation 5 sent · 0 failed · 0 suppressed"]);
  });

  it("says a list is empty only once the read has answered", async () => {
    mountWindow(seeded({ emailStatistics: [statisticsRow({ campaigns: [], audiences: [], automations: [] })] }));

    for (const name of ["Audience growth", "Top campaigns", "Automations"]) {
      const section = await screen.findByRole("region", { name });
      await waitFor(() => expect(section.textContent).toContain("Nothing in this period."));
    }
  });

  it("says a list was not measured, rather than empty, where its figures were not counted", async () => {
    // The rollup has walked the sends, not yet the subscription changes or the opens:
    // their figures are null and their lists arrive empty, which is not "nothing happened".
    mountWindow(seeded({
      emailStatistics: [statisticsRow({
        totals: { subscribes: null, unsubscribes: null, bounces: null, complaints: null, opens: null, clicks: null, uniqueOpens: null, uniqueClicks: null, openRate: null, clickRate: null },
        campaigns: [],
        audiences: [],
        automations: [],
      })],
    }));

    await waitFor(() => expect(figure("sent").value).toBe("1,204"));
    expect(screen.getByRole("region", { name: "Audience growth" }).textContent).toContain("Not measured.");
    expect(screen.getByRole("region", { name: "Top campaigns" }).textContent).toContain("Not measured.");
    // The sends were counted: no automation sent mail.
    expect(screen.getByRole("region", { name: "Automations" }).textContent).toContain("Nothing in this period.");
    expect(pane("overview").textContent).not.toContain("No campaign mail was sent in this period.");
    expect(figure("openRate").value).toBe(DASH);
    expect(figure("subscribes").value).toBe(DASH);
  });

  it("stands every subject's heading on the line the ranked lists give theirs", async () => {
    mountWindow(seeded());
    await waitFor(() => expect(figure("sent").value).toBe("1,204"));

    // The shell's list heading line (`.os-record-heading`: the control line's
    // height, the heading's margin taken off) is the box the kit's ranked lists
    // give Top campaigns and Clients, so the subjects stand alike.
    for (const name of ["Delivery problems", "Audience growth", "Automations"]) {
      const heading = screen.getByRole("heading", { level: 4, name });
      expect(heading.parentElement?.classList.contains("os-record-heading")).toBe(true);
      expect(heading.parentElement?.parentElement?.getAttribute("aria-label")).toBe(name);
    }
    expect(screen.getByRole("heading", { level: 4, name: "Top campaigns" }).closest(".os-ranked-list")).not.toBeNull();
    // A list that is a section of the page is the shell's record section.
    for (const name of ["Audience growth", "Automations"]) {
      expect(screen.getByRole("region", { name }).classList.contains("os-record-section")).toBe(true);
    }
  });

  it("keeps the campaigns by state", async () => {
    mountWindow(seeded());

    const breakdown = await screen.findByRole("region", { name: "Campaigns" });
    // The client in scope's campaigns: every client's, here.
    expect(within(breakdown).getAllByRole("listitem").map((item) => item.textContent)).toEqual(["Draft2", "Sending1", "Sent1"]);
  });
});

describe("a period with nothing to compare", () => {
  it("shows no comparison for twelve months", async () => {
    const conn = seeded({ emailStatistics: [twelveMonthsStatisticsRow()] });
    mountWindow(conn, { settings: { statisticsPeriod: "12m" } });

    await waitFor(() => expect(figure("sent").value).toBe("14,020"));
    expect(lastAsked(conn)).toEqual({ accountIds: [], period: "12m" });
    expect(selectedLabel(screen.getByLabelText("Period"))).toBe("Last 12 months");
    // No change on any figure, and no previous line on any chart.
    const changes = [...document.querySelectorAll(".os-stat-figure-change")].map((node) => node.textContent);
    expect(changes.length).toBe(9);
    expect(changes.every((text) => text === "")).toBe(true);
    expect(document.querySelector("[data-series='previous']")).toBeNull();
    // The current month and the twelve before it.
    expect(mainChart().summary).toMatch(/^Sent: 13 points from Nov 2025 to Nov 2026; /);
  });

  it("shows no change against a previous period that began before counting did", async () => {
    // Counting began on Sep 20; the thirty days before began on Sep 10, ten of them uncounted.
    mountWindow(seeded({ emailStatistics: [statisticsRow({ measuredSince: "2026-09-20" })] }));

    await waitFor(() => expect(figure("sent").value).toBe("1,204"));
    expect([...document.querySelectorAll(".os-stat-figure-change")].every((node) => node.textContent === "")).toBe(true);
    expect(document.querySelector("[data-series='previous']")).toBeNull();
  });
});

describe("whose mail", () => {
  it("lets staff choose among every client, and lists the clients", async () => {
    mountWindow(seeded({ emailStatistics: [statisticsRow({ accounts: TWO_CLIENTS })] }));

    const control = await screen.findByLabelText("Client");
    expect(selectedLabel(control)).toBe("All clients");
    await waitFor(() => expect(within(openSelect(control)).getAllByRole("option").map((option) => option.textContent)).toEqual(["All clients", "Acme", "Borden Ltd", "Operator organization"]));
    chooseOption(control, "All clients");

    const clients = await screen.findByRole("region", { name: "Clients" });
    // By sent, each with its open rate; a client that sent no campaign mail has none to state.
    await waitFor(() => expect(ranked(clients)).toEqual(["Acme | 45% opened | 900", "Borden Ltd | 304"]));
    // Every client is listed, so nothing says some were left out.
    expect(within(clients).queryByText(/busiest of/)).toBeNull();
  });

  it("says when the Clients list shows fewer clients than the read covers", async () => {
    const many = Array.from({ length: 10 }, (_, index) => ({ id: `acct-${index}`, sent: 1000 - index * 10, uniqueOpens: 100, uniqueClicks: 10, openRate: 0.1, clickRate: 0.01 }));
    mountWindow(seeded({ emailStatistics: [statisticsRow({ accounts: many })] }));
    const clients = await screen.findByRole("region", { name: "Clients" });
    await waitFor(() => expect(within(clients).getAllByRole("listitem")).toHaveLength(8));
    expect(within(clients).getByText("The 8 busiest of 10 clients.")).toBeTruthy();
  });

  it("lists the clients under All clients only, not once one client is chosen", async () => {
    // The read here answers two organizations whatever it is asked; the page
    // still draws them only while every client is in scope.
    mountWindow(seeded({ emailStatistics: [statisticsRow({ accounts: TWO_CLIENTS })] }), { settings: { statisticsScope: "acct-b" } });

    const control = await screen.findByLabelText("Client");
    await waitFor(() => expect(selectedLabel(control)).toBe("Borden Ltd"));
    expect(figure("sent").value).toBe("1,204");
    expect(screen.queryByRole("region", { name: "Clients" })).toBeNull();
  });

  it("lists no clients when the read covers one", async () => {
    mountWindow(seeded({ emailStatistics: [statisticsRow({ accounts: [{ id: "acct-a", sent: 900, uniqueOpens: 400, uniqueClicks: 30, openRate: 0.45, clickRate: 0.033 }] })] }));

    await waitFor(() => expect(figure("sent").value).toBe("1,204"));
    expect(screen.queryByRole("region", { name: "Clients" })).toBeNull();
  });

  it("shows a member of one client no client control and no other client", async () => {
    // Even a read that listed two organizations draws no Clients list for
    // somebody who reaches one: the list is All clients' only (spec 9).
    const conn = seeded({ emailStatistics: [statisticsRow({ accounts: TWO_CLIENTS })] });
    mountWindow(conn, { session: { role: "user", everyAccount: false, accountIds: ["acct-a"] } });

    await waitFor(() => expect(figure("sent").value).toBe("1,204"));
    expect(screen.queryByLabelText("Client")).toBeNull();
    expect(screen.queryByRole("region", { name: "Clients" })).toBeNull();
    // Their scope is their organization: the read names none, and the server narrows it.
    expect(lastAsked(conn)).toEqual({ accountIds: [], period: "30d" });
    for (const name of ["Borden Ltd", "Operator organization", "All clients", "All my clients"]) {
      expect(document.body.textContent).not.toContain(name);
    }
  });

  it("sends the chosen client and period to the read, and remembers both", async () => {
    const conn = seeded();
    const { store } = mountWindow(conn);

    const control = await screen.findByLabelText("Client");
    await waitFor(() => expect(within(openSelect(control)).getAllByRole("option")).toHaveLength(4));
    chooseOption(control, "Borden Ltd");

    await waitFor(() => expect(lastAsked(conn)).toEqual({ accountIds: ["acct-b"], period: "30d" }));
    expect(store.load().statisticsScope).toBe("acct-b");

    // Email offers its own periods: no 24 hours, because its facts are counted by the day.
    const period = screen.getByLabelText("Period");
    expect(within(openSelect(period)).getAllByRole("option").map((option) => option.textContent)).toEqual(["Last 7 days", "Last 30 days", "Last 90 days", "Last 12 months"]);
    chooseOption(period, "Last 7 days");
    await waitFor(() => expect(lastAsked(conn)).toEqual({ accountIds: ["acct-b"], period: "7d" }));
    expect(store.load().statisticsPeriod).toBe("7d");
  });

  it("reads under the remembered client from the first read", async () => {
    const conn = seeded();
    mountWindow(conn, { settings: { statisticsScope: "acct-b", statisticsPeriod: "90d" } });

    await waitFor(() => expect(conn.query.emailStatistics).toHaveBeenCalled());
    expect(conn.query.emailStatistics.mock.calls[0]?.[0]).toEqual({ accountIds: ["acct-b"], period: "90d" });
  });

  it("counts what is true now over the chosen client's audiences and automations", async () => {
    mountWindow(seeded(), { settings: { statisticsScope: "acct-b" } });

    await waitFor(() => expect(fact("Subscriptions")).toBe("40"));
    expect(fact("Active automations")).toBe("1");
    const breakdown = await screen.findByRole("region", { name: "Campaigns" });
    expect(within(breakdown).getAllByRole("listitem").map((item) => item.textContent)).toEqual(["Draft1"]);
  });
});

describe("a period nothing counted", () => {
  it("says nothing was measured, never zero", async () => {
    mountWindow(seeded({ emailStatistics: [unmeasuredStatisticsRow()] }));

    await waitFor(() => expect(figure("sent").shaped).toBe(false));
    for (const id of ["sent", "openRate", "clickRate", "unsubscribes", "subscribes"]) {
      expect(figure(id).value).toBe(DASH);
      expect(figure(id).hover).toBe("Nothing has been recorded yet.");
      expect(figure(id).change).toBe("");
    }
    // The chart says it once; the lists of what happened are not drawn over a period nothing counted.
    expect(mainChart().empty).toBe("Not measured");
    for (const name of ["Delivery problems", "Audience growth", "Top campaigns", "Automations", "Clients"]) {
      expect(screen.queryByRole("region", { name })).toBeNull();
    }
    expect(pane("overview").textContent).not.toMatch(/(^|[^\d.,])0%/);
    // What is true now is still said: it was counted.
    await waitFor(() => expect(fact("Subscriptions")).toBe("1,330"));
  });
});

describe("a read that was refused", () => {
  it("says what the engine said, never a zero", async () => {
    mountWindow(seeded({ emailStatistics: new Error("Email statistics are not yours to read.") }));

    await waitFor(() => expect(figure("sent").value).toBe(DASH));
    for (const id of ["sent", "openRate", "clickRate", "unsubscribes", "subscribes"]) {
      expect(figure(id).value).toBe(DASH);
      expect(figure(id).hover).toBe("Email statistics are not yours to read.");
    }
    expect(notices()).toEqual(["This cluster did not return the email statistics.Email statistics are not yours to read."]);
    // No chart of a read that did not come back, and no figure to choose for one.
    expect(document.querySelector(".os-time-chart")).toBeNull();
    expect(document.querySelector("[role='radio']")).toBeNull();
    expect(pane("overview").textContent).not.toMatch(/(^|[^\d.,])0%/);
    // What did answer still does.
    await waitFor(() => expect(fact("Subscriptions")).toBe("1,330"));
  });

  it("does not say the subscriptions were refused when only an audience the growth list names was", async () => {
    // Old list is archived: it is counted only for its growth row, and its
    // count is refused. Subscriptions, of the active audiences, was counted.
    mountWindow(seeded({
      subscriberCounts: { "a-news": 1204, "a-signups": 86, "a-borden": 40, "a-old": new Error("Counting subscribers is not yours to do.") },
      emailStatistics: [statisticsRow({ audiences: [{ id: "a-news", subscribes: 12, unsubscribes: 3, net: 9 }, { id: "a-old", subscribes: 0, unsubscribes: 4, net: -4 }] })],
    }));

    await waitFor(() => expect(fact("Subscriptions")).toBe("1,330"));
    const growth = screen.getByRole("region", { name: "Audience growth" });
    await waitFor(() => expect(records(growth)).toEqual([
      "Newsletter 12 new · 3 unsubscribed · +9 net · 1,204 subscribed",
      // Its size could not be had: left out, never a zero.
      "Old list 0 new · 4 unsubscribed · −4 net",
    ]));
    expect(notices()).toEqual([]);
    expect(pane("overview").textContent).not.toContain("This cluster did not return your subscriptions.");
  });

  it("aborts the read a new period or client replaces, and never draws its late answer", async () => {
    const conn = seeded();
    const asked: { args: unknown; signal: AbortSignal; answer: (row: Row) => void }[] = [];
    conn.query.emailStatistics.mockImplementation((args, opts) => new Promise((resolve) => {
      asked.push({ args, signal: (opts as { signal: AbortSignal }).signal, answer: (row) => resolve(rowsResult([row])) });
    }));
    mountWindow(conn);
    await waitFor(() => expect(asked).toHaveLength(1));
    expect(asked[0]?.args).toEqual({ accountIds: [], period: "30d" });

    // A new period: the thirty days' read is stale from the moment the week is asked for.
    chooseOption(screen.getByLabelText("Period"), "Last 7 days");
    await waitFor(() => expect(asked).toHaveLength(2));
    expect(asked[1]?.args).toEqual({ accountIds: [], period: "7d" });
    expect(asked[0]?.signal.aborted).toBe(true);
    expect(asked[1]?.signal.aborted).toBe(false);
    await act(async () => asked[1]?.answer(statisticsRow({ period: "7d", totals: { sent: 280 } })));
    await waitFor(() => expect(figure("sent").value).toBe("280"));
    // The thirty days answer late: none of it is drawn.
    await act(async () => asked[0]?.answer(statisticsRow({ totals: { sent: 9999 } })));
    expect(figure("sent").value).toBe("280");

    // A new client, chosen while the one before is still being read.
    const control = await screen.findByLabelText("Client");
    await waitFor(() => expect(within(openSelect(control)).getAllByRole("option")).toHaveLength(4));
    chooseOption(control, "Borden Ltd");
    await waitFor(() => expect(asked).toHaveLength(3));
    chooseOption(control, "Acme");
    await waitFor(() => expect(asked).toHaveLength(4));
    expect(asked[2]?.args).toEqual({ accountIds: ["acct-b"], period: "7d" });
    expect(asked[3]?.args).toEqual({ accountIds: ["acct-a"], period: "7d" });
    expect(asked[2]?.signal.aborted).toBe(true);
    await act(async () => asked[3]?.answer(statisticsRow({ period: "7d", totals: { sent: 600 } })));
    await waitFor(() => expect(figure("sent").value).toBe("600"));
    await act(async () => asked[2]?.answer(statisticsRow({ period: "7d", totals: { sent: 50 } })));
    expect(figure("sent").value).toBe("600");
  });

  it("keeps the last figures through a refused refresh, and says on the heading's line that updates are interrupted", async () => {
    const conn = seeded();
    standalone(conn);
    await waitFor(() => expect(figure("sent").value).toBe("1,204"));
    expect(headNote()).toBeNull();

    conn.query.emailStatistics.mockRejectedValue(new Error("The cluster did not answer in time."));
    // Coming back to the window reads the statistics again, and the cluster refuses.
    await act(async () => { window.dispatchEvent(new Event("focus")); });

    await waitFor(() => expect(headNote()?.textContent).toBe("Updates are interrupted. Showing the last figures received."));
    expect(headNote()?.getAttribute("title")).toBe("The cluster did not answer in time.");
    expect(figure("sent").value).toBe("1,204");
    expect(mainChart().label).toBe("Sent");
    expect(notices()).toEqual([]);
  });

  it("says it is reconnecting, and claims no figures, when a list stopped before it answered", async () => {
    const conn = seeded();
    // The lists are held back, so nothing of theirs was ever received.
    let release!: () => void;
    const gate = new Promise<void>((done) => { release = done; });
    for (const read of [conn.query.campaigns, conn.query.audiences, conn.query.emailRules, conn.query.journeys]) {
      const answer = read.getMockImplementation()!;
      read.mockImplementation(async (args, opts) => { await gate; return answer(args, opts); });
    }
    const held = standalone(conn);
    await act(async () => {});

    act(() => (held.feeds!.campaigns.source as unknown as { markDisconnected: () => void }).markDisconnected());

    await waitFor(() => expect(headNote()?.textContent).toBe("Updates are interrupted. Reconnecting automatically."));
    // Nothing has been counted, so nothing is claimed: no first-use offer either.
    expect(screen.queryByText("Nothing sent yet")).toBeNull();
    expect(notices()).toEqual([]);
    await act(async () => release());
  });
});

describe("first use", () => {
  it("first use offers Add a campaign", async () => {
    const { navigate } = mountWindow(fakeConnection({ emailStatistics: [unmeasuredStatisticsRow()] }));

    expect(await screen.findByRole("heading", { level: 4, name: "Nothing sent yet" })).toBeTruthy();
    expect(screen.getByText("Add a campaign to send a template to an audience.")).toBeTruthy();
    // Nobody is subscribed and nothing runs on its own: real zeros. Nothing was counted, so the figures are dashes.
    await waitFor(() => expect(fact("Subscriptions")).toBe("0"));
    expect(fact("Active automations")).toBe("0");
    await waitFor(() => expect(figure("sent").value).toBe(DASH));
    // "Nothing sent yet" says it: no chart of an empty period, no lists, no breakdown.
    expect(document.querySelector(".os-time-chart")).toBeNull();
    expect(screen.queryByRole("region", { name: "Delivery problems" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Campaigns" })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Add a campaign" }));

    // The Campaigns tab, on its own wizard.
    expect(navigate).toHaveBeenCalledWith("campaigns", { fromContent: true });
    expect(await screen.findByRole("heading", { level: 3, name: "Add a campaign" })).toBeTruthy();
    expect(pane("overview").hidden).toBe(true);
  });

  it("is not offered once there is a campaign, or an automation, to speak of", async () => {
    const withCampaign = mountWindow(fakeConnection({ campaigns: [campaignRow({ id: "d" })], emailStatistics: [statisticsRow()] }));
    await waitFor(() => expect(screen.getByRole("region", { name: "Campaigns" })).toBeTruthy());
    expect(screen.queryByText("Nothing sent yet")).toBeNull();
    withCampaign.view.unmount();

    // A paused automation is still an automation.
    const withRule = mountWindow(fakeConnection({ emailRules: [ruleRow({ id: "r", status: "paused" })], emailStatistics: [statisticsRow()] }));
    await waitFor(() => expect(figure("sent").value).toBe("1,204"));
    expect(screen.queryByText("Nothing sent yet")).toBeNull();
    withRule.view.unmount();
  });

  it("is not offered to a client whose only automation is a journey", async () => {
    // A welcome series and nothing else: its sends are in the period's figures,
    // so "Nothing sent yet" beside a Sent above zero would contradict them.
    mountWindow(fakeConnection({
      journeys: [journeyRow({ id: "j-series", name: "Welcome series", status: "active" })],
      emailStatistics: [statisticsRow({ campaigns: [], automations: [{ id: "j-series", kind: "journey", sent: 40, failed: 0, suppressed: 0 }] })],
    }));

    await waitFor(() => expect(figure("sent").value).toBe("1,204"));
    expect(screen.queryByText("Nothing sent yet")).toBeNull();
    // The period is drawn: its chart, and the journey among the automations.
    expect(mainChart().label).toBe("Sent");
    await waitFor(() => expect(records(screen.getByRole("region", { name: "Automations" }))).toEqual(["Welcome series 40 sent · 0 failed · 0 suppressed"]));
    expect(figure("sent").checked).toBe("true");
  });
});

describe("the page's help", () => {
  it("says what a rate is of and what is not counted, once, without repeating the figures", async () => {
    mountWindow(seeded());
    await waitFor(() => expect(figure("sent").value).toBe("1,204"));

    const help = pane("overview").querySelector(".os-page-help-inline");
    expect(help?.querySelector("summary")?.textContent).toBe("Overview");
    const said = [...(help?.querySelectorAll("p") ?? [])].map((node) => node.textContent ?? "");
    expect(said).toEqual([
      "A rate is the share of the campaign messages sent in the period that have been opened or clicked.",
      "Automations show no opens or clicks, because automated mail is not tracked for them.",
      "A recipient counts once per campaign, however often they open or click it.",
      "Mail privacy proxies open messages before their readers do, so opens are an upper bound.",
      "Days are counted in UTC.",
    ]);
    for (const label of ["Sent", "Open rate", "Click rate", "Unsubscribes", "New subscribers", "Bounces", "Complaints", "Failed", "Suppressed"]) {
      expect(said.join(" ")).not.toContain(label);
    }
  });
});

describe("how the Overview reads", () => {
  it("adds no subscription of its own", async () => {
    const conn = seeded();
    mountWindow(conn);
    await waitFor(() => expect(figure("sent").value).toBe("1,204"));
    for (const [concept, peak] of peakSubscriptions(conn)) expect(peak, concept).toBeLessThanOrEqual(1);
  });

  it("reads the statistics again every fifteen minutes while it is seen, and stops when another tab is", async () => {
    expect(STATISTICS_REFRESH_MS).toBe(900_000);
    // What is true now is counted again each minute, as before.
    expect(OVERVIEW_REFRESH_MS).toBe(60_000);
    const conn = seeded();
    h.connection = conn;
    const store = memoryStore();
    const app = (section: string) =>
      withSession(<EmailApp sectionId={section} navigate={() => {}} askContext={() => {}} store={store} uploads={{ upload: vi.fn() }} />);
    const view = render(app("overview"));
    await waitFor(() => expect(figure("sent").value).toBe("1,204"));
    await act(async () => {});

    const reads = () => conn.query.emailStatistics.mock.calls.length;
    vi.useFakeTimers();
    // Coming back to the window arms the refresher on the clock this test holds.
    await act(async () => window.dispatchEvent(new Event("focus")));
    const armed = reads();

    await act(async () => { await vi.advanceTimersByTimeAsync(STATISTICS_REFRESH_MS - 1_000); });
    expect(reads()).toBe(armed);
    await act(async () => { await vi.advanceTimersByTimeAsync(1_000); });
    expect(reads()).toBe(armed + 1);

    // The person goes to Campaigns. The Overview stays mounted behind it, and stops reading.
    view.rerender(app("campaigns"));
    expect(pane("overview").hidden).toBe(true);
    const left = reads();
    await act(async () => { await vi.advanceTimersByTimeAsync(STATISTICS_REFRESH_MS * 3); });
    expect(reads()).toBe(left);
  });

  it("stops reading when the window closes: an unmount aborts the read it was waiting for", async () => {
    const conn = seeded();
    const signals: AbortSignal[] = [];
    conn.query.emailStatistics.mockImplementation(async (_args, opts) => {
      signals.push((opts as { signal: AbortSignal }).signal);
      return new Promise<never>(() => {});
    });
    const { view } = mountWindow(conn);
    await waitFor(() => expect(signals.length).toBeGreaterThan(0));
    expect(signals.every((signal) => !signal.aborted)).toBe(true);

    view.unmount();

    expect(signals.every((signal) => signal.aborted)).toBe(true);
  });
});
