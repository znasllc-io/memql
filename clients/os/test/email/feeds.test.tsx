import { act, fireEvent, render, renderHook, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

vi.mock("../../src/auth/context", () => ({
  useAuthSource: () => ({ bearer: async () => "test-token", refresh: async () => {} }),
}));

const { EmailApp } = await import("../../src/apps/email/EmailApp");
const { LocalEmailSettingsStore } = await import("../../src/apps/email/settings");
const { useSeriesFeed, useSignupFormFeed, useSubscriberCounts, useTestSettingsFeed } = await import("../../src/apps/email/useEmail");
const { absent, figureOf } = await import("../../src/kit/measure");
const { ACCOUNT_CONCEPT } = await import("../../src/apps/accounts/rows");
const { SITE_CONCEPT } = await import("../../src/apps/deployables/concepts");
const {
  audienceRow,
  campaignRow,
  fakeConnection,
  liveSubscriptions,
  pagedResult,
  peakSubscriptions,
  rowsResult,
  withSession,
} = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

const SERIES = "v1:campaigns:campaignSeries";
const TEST_SETTINGS = "v1:campaigns:testSettings";
const SIGNUP_FORM = "v1:campaigns:newsletterBinding";

function memoryStore(over: Record<string, unknown> = {}) {
  const bag = new Map<string, string>();
  const store = new LocalEmailSettingsStore({
    getItem: (k: string) => bag.get(k) ?? null,
    setItem: (k: string, v: string) => void bag.set(k, v),
  });
  if (Object.keys(over).length > 0) store.save({ ...store.load(), ...over });
  return store;
}

/** The app as a shell would mount it; `section()` re-renders the SAME window on
 *  another section, which is what a person clicking a tab does. */
function mount(connection: Conn, sectionId = "campaigns", settings: Record<string, unknown> = {}) {
  h.connection = connection;
  const store = memoryStore(settings);
  const uploads = { upload: vi.fn() };
  const app = (section: string) =>
    withSession(
      <EmailApp sectionId={section} navigate={() => {}} askContext={() => {}} store={store} uploads={uploads} />,
    );
  const view = render(app(sectionId));
  return { view, section: (next: string) => view.rerender(app(next)) };
}

/** One retained section's own box. A section the person has left is hidden,
 *  not gone, so text that two sections share is only unique within its pane. */
function pane(id: string): HTMLElement {
  const node = document.querySelector<HTMLElement>(`[data-email-section="${id}"]`);
  if (node === null) throw new Error(`the ${id} section is not mounted`);
  return node;
}

function headMeta(title: string): string | null {
  return screen.getByRole("heading", { level: 3, name: title }).parentElement?.querySelector(".os-head-meta")?.textContent ?? null;
}

function opened(connection: Conn, concept: string): number {
  return connection.subscriptions.subscribeGraph.mock.calls.filter(([, options]) => options.concept === concept).length;
}

// ---------------------------------------------------------------------------
// Every page
// ---------------------------------------------------------------------------

describe("the root feeds read every page", () => {
  const named = (from: number, to: number) =>
    Array.from({ length: to - from + 1 }, (_, i) =>
      campaignRow({ id: `v1:campaigns:campaign:c${from + i}`, name: `Campaign ${String(from + i).padStart(2, "0")}` }),
    );

  it("lists every page of campaigns, not the first fifty", async () => {
    const conn = fakeConnection();
    conn.query.campaigns
      .mockResolvedValueOnce(pagedResult(named(1, 50), "p2"))
      .mockResolvedValueOnce(rowsResult(named(51, 53)));
    mount(conn);

    expect(await screen.findByText("Campaign 53")).toBeTruthy();
    for (let n = 1; n <= 53; n++) expect(screen.getByText(`Campaign ${String(n).padStart(2, "0")}`)).toBeTruthy();
    await waitFor(() => expect(headMeta("Campaigns")).toBe("53"));

    // The second page is asked for by the first page's cursor, inside the one
    // seed -- the subscription is not torn down and reopened to page.
    expect(conn.query.campaigns).toHaveBeenCalledTimes(2);
    expect(conn.query.campaigns.mock.calls[0]![1]).not.toHaveProperty("cursor");
    expect(conn.query.campaigns.mock.calls[1]![1]).toMatchObject({ cursor: "p2" });
    expect(opened(conn, "v1:campaigns:campaign")).toBe(1);
  });

  it("walks the cursor of the other four feeds too", async () => {
    const conn = fakeConnection();
    const feeds = ["audiences", "templates", "senderIdentities", "emailRules"] as const;
    const row = (id: string) => ({ id, ownerUserId: "v1:identity:user:me", name: id, accountId: "self", status: "active", address: `${id}@acme.test`, createdAt: "2026-08-01T00:00:00Z" });
    for (const feed of feeds) {
      conn.query[feed]
        .mockResolvedValueOnce(pagedResult([row(`${feed}-first`)], `${feed}-next`))
        .mockResolvedValueOnce(rowsResult([row(`${feed}-second`)]));
    }
    mount(conn);
    for (const feed of feeds) {
      await waitFor(() => expect(conn.query[feed]).toHaveBeenCalledTimes(2));
      expect(conn.query[feed].mock.calls[0]![1]).not.toHaveProperty("cursor");
      expect(conn.query[feed].mock.calls[1]![1]).toMatchObject({ cursor: `${feed}-next` });
    }
  });
});

// ---------------------------------------------------------------------------
// One subscription per concept
// ---------------------------------------------------------------------------

describe("one subscription per concept", () => {
  const seed = () => ({
    campaigns: [campaignRow({ id: "c1", name: "August update", audienceId: "a1" })],
    audiences: [audienceRow({ id: "a1", name: "Newsletter" })],
    sites: [{ id: "site1", accountId: "self", title: "Storefront", status: "live" }],
    newsletters: [{ id: "n1", siteId: "site1", accountId: "self", audienceId: "a1", templateId: "t1", senderIdentityId: "s1", enabled: true, consentText: "Email me the newsletter.", createdAt: "2026-10-01T12:00:00Z" }],
    testSettings: [{ id: "ts1", accountId: "self", audienceId: "a1", createdAt: "2026-10-01T12:00:00Z" }],
    campaignSeries: [{ id: "s1", sourceCampaignId: "c1", status: "active", intervalWeeks: 2, timeZone: "UTC", anchorAt: "2028-01-02T16:00:00Z", nextAt: "2028-01-16T16:00:00Z", createdAt: "2027-10-01T12:00:00Z" }],
  });

  function noConceptHasTwo(connection: Conn) {
    for (const [concept, open] of liveSubscriptions(connection)) expect(open, concept).toBeLessThanOrEqual(1);
  }

  it("subscribes once per concept however many pages are open", async () => {
    const conn = fakeConnection(seed());
    const { section } = mount(conn, "campaigns");

    // A campaign page: its series (the Repeat panel) and its organization's testing settings.
    fireEvent.click(await within(pane("campaigns")).findByText("August update"));
    expect(await within(pane("campaigns")).findByText(/Every 2 weeks/)).toBeTruthy();
    await waitFor(() => expect(liveSubscriptions(conn).get(TEST_SETTINGS)).toBe(1));
    noConceptHasTwo(conn);

    // An audience page: its signup forms. The campaign page stays open behind it.
    section("audiences");
    fireEvent.click(await within(pane("audiences")).findByText("Newsletter"));
    expect(await within(pane("audiences")).findByRole("button", { name: /Storefront/ })).toBeTruthy();
    await waitFor(() => expect(liveSubscriptions(conn).get(SIGNUP_FORM)).toBe(1));
    noConceptHasTwo(conn);

    // Settings, with both pages still open: the testing settings and the account list again.
    section("settings");
    expect(await within(pane("settings")).findByText("Testing audience", { selector: "h4" })).toBeTruthy();
    noConceptHasTwo(conn);

    const open = liveSubscriptions(conn);
    for (const concept of [SERIES, TEST_SETTINGS, SIGNUP_FORM, ACCOUNT_CONCEPT, SITE_CONCEPT, "v1:campaigns:campaign", "v1:campaigns:audience"]) {
      expect(open.get(concept), concept).toBe(1);
    }
    // And never two at ANY moment on the way -- a restart that lingers between
    // two assertions would not show in the photographs above.
    for (const [concept, most] of peakSubscriptions(conn)) expect(most, concept).toBeLessThanOrEqual(1);
  });

  it("keeps one subscription when the ids in view change", async () => {
    const conn = fakeConnection(seed());
    mount(conn, "campaigns");
    await within(pane("campaigns")).findByText("August update");
    await waitFor(() => expect(conn.query.campaignSeriesForCampaign).toHaveBeenCalledTimes(1));

    // A second draft arrives while somebody watches.
    await act(async () => {
      conn.subscriptions.emit("v1:campaigns:campaign", campaignRow({ id: "c2", name: "Autumn offer" }), "NODE_CREATED");
    });
    await within(pane("campaigns")).findByText("Autumn offer");
    await waitFor(() => expect(conn.query.campaignSeriesForCampaign).toHaveBeenCalledWith({ campaignId: "c2" }, expect.anything()));

    expect(opened(conn, SERIES)).toBe(1);
    expect(peakSubscriptions(conn).get(SERIES)).toBe(1);
  });

  it("keeps a visited section mounted and hidden, with its page open", async () => {
    const conn = fakeConnection(seed());
    const { section } = mount(conn, "campaigns");
    fireEvent.click(await within(pane("campaigns")).findByText("August update"));
    await within(pane("campaigns")).findByRole("region", { name: "August update progress" });

    section("audiences");
    await within(pane("audiences")).findByText("Newsletter");
    expect(pane("campaigns").hidden).toBe(true);
    expect(pane("audiences").hidden).toBe(false);

    section("campaigns");
    expect(pane("campaigns").hidden).toBe(false);
    expect(pane("audiences").hidden).toBe(true);
    // The campaign page is exactly where it was left, not a fresh list.
    expect(within(pane("campaigns")).getByRole("region", { name: "August update progress" })).toBeTruthy();
    expect(conn.query.campaigns).toHaveBeenCalledTimes(1);
  });
});

// ---------------------------------------------------------------------------
// The composed feeds
// ---------------------------------------------------------------------------

describe("composed feeds", () => {
  it("one series feed answers every draft", async () => {
    const conn = fakeConnection({
      campaigns: [
        campaignRow({ id: "d1", name: "Welcome series" }),
        campaignRow({ id: "d2", name: "Autumn offer" }),
        campaignRow({ id: "s1", name: "Already sent", status: "sent" }),
      ],
      campaignSeries: [{ id: "series-1", sourceCampaignId: "d1", status: "active", intervalWeeks: 3, timeZone: "UTC", anchorAt: "2028-01-02T16:00:00Z", nextAt: "2028-01-23T16:00:00Z", createdAt: "2027-10-01T12:00:00Z" }],
    });
    mount(conn, "campaigns", { showFiled: true });
    await within(pane("campaigns")).findByText("Welcome series");
    await waitFor(() => expect(conn.query.campaignSeriesForCampaign).toHaveBeenCalledTimes(2));

    // One subscription, and one read per DRAFT: only a draft can source a series.
    expect(opened(conn, SERIES)).toBe(1);
    const asked = conn.query.campaignSeriesForCampaign.mock.calls.map(([args]) => (args as { campaignId: string }).campaignId).sort();
    expect(asked).toEqual(["d1", "d2"]);

    // Opening a draft reads nothing more: the panel reads the root's feed.
    fireEvent.click(within(pane("campaigns")).getByText("Welcome series"));
    expect(await within(pane("campaigns")).findByText(/Every 3 weeks/)).toBeTruthy();
    expect(conn.query.campaignSeriesForCampaign).toHaveBeenCalledTimes(2);
    expect(opened(conn, SERIES)).toBe(1);
  });

  it("answers a draft with no series with nothing, not with another draft's series", async () => {
    const conn = fakeConnection({
      campaigns: [campaignRow({ id: "d1", name: "Welcome series" }), campaignRow({ id: "d2", name: "Autumn offer" })],
      campaignSeries: [{ id: "series-1", sourceCampaignId: "d1", status: "active", intervalWeeks: 3, timeZone: "UTC", anchorAt: "2028-01-02T16:00:00Z", nextAt: "2028-01-23T16:00:00Z", createdAt: "2027-10-01T12:00:00Z" }],
    });
    mount(conn, "campaigns");
    await within(pane("campaigns")).findByText("Autumn offer");
    await waitFor(() => expect(conn.query.campaignSeriesForCampaign).toHaveBeenCalledTimes(2));
    fireEvent.click(within(pane("campaigns")).getByText("Autumn offer"));
    expect(await within(pane("campaigns")).findByRole("button", { name: /Set up repeating sends/ })).toBeTruthy();
    expect(within(pane("campaigns")).queryByText(/Every 3 weeks/)).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// One campaign page
// ---------------------------------------------------------------------------

describe("one campaign page", () => {
  it("asks whether sending is set up once, and hands the answer to every panel", async () => {
    const conn = fakeConnection({
      campaigns: [campaignRow({ id: "c1", name: "August update" })],
      sendingReadiness: [{ ready: false, reason: "Sending isn't set up for Acme.", capture: false }],
    });
    mount(conn);
    fireEvent.click(await within(pane("campaigns")).findByText("August update"));
    // Both the send controls and the test panel say so -- from the one answer.
    expect(await within(pane("campaigns")).findByText("Sending setup is incomplete or unconfirmed.")).toBeTruthy();
    expect(within(pane("campaigns")).getByText("Finish sending setup before sending a test.")).toBeTruthy();
    await act(async () => {});
    expect(conn.query.campaignSendingReadiness).toHaveBeenCalledTimes(1);
    expect(conn.query.campaignSendingReadiness).toHaveBeenCalledWith({ campaignId: "c1" }, expect.anything());
  });

  it("offers a repeating schedule to a draft and to nothing else", async () => {
    const conn = fakeConnection({
      campaigns: [
        campaignRow({ id: "d1", name: "Draft one" }),
        campaignRow({ id: "s1", name: "Already sent", status: "sent" }),
      ],
    });
    const { view } = mount(conn, "campaigns", { showFiled: true });
    fireEvent.click(await within(pane("campaigns")).findByText("Draft one"));
    expect(await within(pane("campaigns")).findByRole("region", { name: "Repeating campaign" })).toBeTruthy();

    view.unmount();
    mount(conn, "campaigns", { showFiled: true });
    fireEvent.click(await within(pane("campaigns")).findByText("Already sent"));
    await within(pane("campaigns")).findByRole("region", { name: "Already sent progress" });
    expect(within(pane("campaigns")).queryByRole("region", { name: "Repeating campaign" })).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// The composed hooks, one at a time
// ---------------------------------------------------------------------------

describe("a composed feed", () => {
  const series = (campaignId: string, id = `series-${campaignId}`) => ({
    id, sourceCampaignId: campaignId, status: "active", intervalWeeks: 2, timeZone: "UTC",
    anchorAt: "2028-01-02T16:00:00Z", nextAt: "2028-01-16T16:00:00Z", createdAt: "2027-10-01T12:00:00Z",
  });
  const sourcesOf = (rows: readonly Record<string, unknown>[]) => rows.map((row) => row.sourceCampaignId).sort();

  it("holds only the rows of the ids it was asked about", async () => {
    const conn = fakeConnection({ campaignSeries: [series("c1")] });
    h.connection = conn;
    const { result } = renderHook(() => useSeriesFeed(["c1"]));
    await waitFor(() => expect(result.current.snapshot.state).toBe("live"));

    // The stream carries every campaign's series to every subscriber of the
    // concept; another campaign's is not this feed's to hold.
    await act(async () => conn.subscriptions.emit(SERIES, series("c2")));
    expect(sourcesOf(result.current.snapshot.rows)).toEqual(["c1"]);

    // A change to a campaign it holds folds in.
    await act(async () => conn.subscriptions.emit(SERIES, { ...series("c1"), status: "paused" }));
    expect(result.current.snapshot.rows[0]).toMatchObject({ status: "paused" });
  });

  it("reads again when the set of ids changes, and keeps its one subscription", async () => {
    const conn = fakeConnection({ campaignSeries: [series("c1"), series("c2")] });
    h.connection = conn;
    const { result, rerender } = renderHook(({ ids }) => useSeriesFeed(ids), { initialProps: { ids: ["c1"] } });
    await waitFor(() => expect(sourcesOf(result.current.snapshot.rows)).toEqual(["c1"]));

    rerender({ ids: ["c2", "c1"] });
    await waitFor(() => expect(sourcesOf(result.current.snapshot.rows)).toEqual(["c1", "c2"]));
    // The same set in another order, or as a new array, is no change at all.
    const reads = conn.query.campaignSeriesForCampaign.mock.calls.length;
    rerender({ ids: ["c1", "c2"] });
    await act(async () => {});
    expect(conn.query.campaignSeriesForCampaign).toHaveBeenCalledTimes(reads);

    rerender({ ids: ["c2"] });
    await waitFor(() => expect(sourcesOf(result.current.snapshot.rows)).toEqual(["c2"]));
    expect(opened(conn, SERIES)).toBe(1);
    expect(peakSubscriptions(conn).get(SERIES)).toBe(1);
  });

  it("reads each organization's testing settings once", async () => {
    const conn = fakeConnection({ testSettings: [{ id: "ts1", accountId: "acme", audienceId: "a1" }, { id: "ts2", accountId: "other", audienceId: "a2" }] });
    h.connection = conn;
    const { result } = renderHook(() => useTestSettingsFeed(["acme", "other", "acme", ""]));
    await waitFor(() => expect(result.current.snapshot.rows).toHaveLength(2));
    expect(conn.query.campaignTestSettings).toHaveBeenCalledTimes(2);
    expect(opened(conn, TEST_SETTINGS)).toBe(1);
  });

  it("walks every page of an audience's signup forms", async () => {
    const conn = fakeConnection();
    const binding = (id: string) => ({ id, siteId: id, audienceId: "a1", enabled: true, createdAt: "2026-10-01T12:00:00Z" });
    conn.query.newslettersForAudience
      .mockResolvedValueOnce(pagedResult([binding("n1")], "more"))
      .mockResolvedValueOnce(rowsResult([binding("n2")]));
    h.connection = conn;
    const { result } = renderHook(() => useSignupFormFeed(["a1"]));
    await waitFor(() => expect(result.current.snapshot.rows).toHaveLength(2));
    expect(conn.query.newslettersForAudience.mock.calls[0]![1]).not.toHaveProperty("cursor");
    expect(conn.query.newslettersForAudience.mock.calls[1]![1]).toMatchObject({ cursor: "more" });
  });

  it("says so, in the engine's sentence, when it could not read every id", async () => {
    const conn = fakeConnection({ testSettings: [{ id: "ts1", accountId: "acme", audienceId: "a1" }] });
    conn.query.campaignTestSettings.mockImplementation(async (args) => {
      if ((args as { accountId?: string }).accountId === "other") throw new Error("Reading testing settings is not yours to do.");
      return rowsResult([{ id: "ts1", accountId: "acme", audienceId: "a1" }]);
    });
    h.connection = conn;
    const { result } = renderHook(() => useTestSettingsFeed(["acme", "other"]));
    // A feed that could not read one id does not claim to hold the others as
    // the whole answer: the failure is the feed's, and it is not a zero.
    await waitFor(() => expect(result.current.snapshot.error).toBe("Reading testing settings is not yours to do."));
  });

  it("reads a crowd of ids a few at a time", async () => {
    const conn = fakeConnection();
    let active = 0;
    let most = 0;
    conn.query.campaignSeriesForCampaign.mockImplementation(async () => {
      active++;
      most = Math.max(most, active);
      await new Promise((done) => setTimeout(done, 2));
      active--;
      return rowsResult([]);
    });
    h.connection = conn;
    const ids = Array.from({ length: 25 }, (_, i) => `c${i}`);
    const { result } = renderHook(() => useSeriesFeed(ids));
    await waitFor(() => expect(conn.query.campaignSeriesForCampaign).toHaveBeenCalledTimes(25));
    await waitFor(() => expect(result.current.snapshot.state).toBe("live"));
    expect(most).toBeGreaterThan(1);
    expect(most).toBeLessThanOrEqual(6);
  });
});

// ---------------------------------------------------------------------------
// Subscriber counts
// ---------------------------------------------------------------------------

describe("subscriber counts", () => {
  function deferred(connection: Conn) {
    let release!: () => void;
    const gate = new Promise<void>((done) => { release = done; });
    const answer = connection.query.audienceSubscriberCount.getMockImplementation()!;
    connection.query.audienceSubscriberCount.mockImplementation(async (args, opts) => { await gate; return answer(args, opts); });
    return release;
  }

  it("counts subscribers per audience and keeps an unread count absent", async () => {
    const conn = fakeConnection({ subscriberCounts: { a1: 7, a2: new Error("Counting subscribers is not yours to do.") } });
    const release = deferred(conn);
    h.connection = conn;
    const { result } = renderHook(() => useSubscriberCounts(["a1", "a2"]));

    // Before the read resolves: not read yet -- never a zero.
    expect(result.current.counts.get("a1")).toEqual({ kind: "absent", reason: "unread", detail: "" });
    expect(result.current.counts.get("a2")).toEqual(absent("unread"));

    await act(async () => { release(); });
    await waitFor(() => expect(result.current.counts.get("a1")).toEqual({ kind: "measured", value: 7 }));
    expect(result.current.counts.get("a1")).toEqual(figureOf(7));
    // A refusal is that audience's own answer, in the engine's own sentence.
    expect(result.current.counts.get("a2")).toEqual(absent("failed", "Counting subscribers is not yours to do."));
    expect(conn.query.audienceSubscriberCount).toHaveBeenCalledTimes(2);
  });

  it("reads again on reload and drops an audience that left the set", async () => {
    const conn = fakeConnection({ subscriberCounts: { a1: 7, a2: 3 } });
    h.connection = conn;
    const { result, rerender } = renderHook(({ ids }) => useSubscriberCounts(ids), { initialProps: { ids: ["a1", "a2"] } });
    await waitFor(() => expect(result.current.counts.get("a2")).toEqual(figureOf(3)));

    conn.query.audienceSubscriberCount.mockImplementation(async (args) => rowsResult([{ count: (args.audienceId === "a1" ? 8 : 4) }]));
    await act(async () => { await result.current.reload(); });
    expect(result.current.counts.get("a1")).toEqual(figureOf(8));
    expect(result.current.counts.get("a2")).toEqual(figureOf(4));

    rerender({ ids: ["a1"] });
    expect([...result.current.counts.keys()]).toEqual(["a1"]);
  });

  it("does not turn a missing count into a zero", async () => {
    const conn = fakeConnection();
    conn.query.audienceSubscriberCount.mockImplementation(async () => rowsResult([]));
    h.connection = conn;
    const { result } = renderHook(() => useSubscriberCounts(["a1"]));
    await waitFor(() => expect(result.current.counts.get("a1")).toEqual(absent("unmeasured")));
  });
});
