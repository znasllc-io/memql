import { act, fireEvent, render, renderHook, screen, waitFor, within } from "@testing-library/react";
import { useLayoutEffect } from "react";
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
const { knowledgeOf, listKnowledge, useEmailFeeds, useSeriesFeed, useSignupFormFeed, useSubscriberCounts, useTestSettingsFeed, weakest } = await import("../../src/apps/email/useEmail");
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
  templateRow,
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

/** A pane's action bar: the state in a word, and the acts it offers. */
function bar(id: string): HTMLElement {
  return within(pane(id)).getByRole("group", { name: "What you can do with this" });
}
const barActs = (id: string) => within(bar(id)).queryAllByRole("button").map((button) => button.textContent);

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

    // A campaign page: its series (it repeats) and its organization's testing settings.
    fireEvent.click(await within(pane("campaigns")).findByText("August update"));
    expect(await within(pane("campaigns")).findByText(/^Every 2 weeks from/)).toBeTruthy();
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
    expect(await within(pane("settings")).findByRole("heading", { level: 4, name: "Testing" })).toBeTruthy();
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
    await within(pane("campaigns")).findByRole("region", { name: "Campaign details" });

    section("audiences");
    await within(pane("audiences")).findByText("Newsletter");
    expect(pane("campaigns").hidden).toBe(true);
    expect(pane("audiences").hidden).toBe(false);

    section("campaigns");
    expect(pane("campaigns").hidden).toBe(false);
    expect(pane("audiences").hidden).toBe(true);
    // The campaign page is exactly where it was left, not a fresh list.
    expect(within(pane("campaigns")).getByRole("region", { name: "Campaign details" })).toBeTruthy();
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
    mount(conn, "campaigns", { includeArchived: true });
    await within(pane("campaigns")).findByText("Welcome series");
    await waitFor(() => expect(conn.query.campaignSeriesForCampaign).toHaveBeenCalledTimes(2));

    // One subscription, and one read per DRAFT: only a draft can source a series.
    expect(opened(conn, SERIES)).toBe(1);
    const asked = conn.query.campaignSeriesForCampaign.mock.calls.map(([args]) => (args as { campaignId: string }).campaignId).sort();
    expect(asked).toEqual(["d1", "d2"]);

    // Opening a draft reads nothing more: its page reads the root's feed.
    fireEvent.click(within(pane("campaigns")).getByText("Welcome series"));
    expect(await within(pane("campaigns")).findByText(/^Every 3 weeks from/)).toBeTruthy();
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
    await waitFor(() => expect(barActs("campaigns")).toContain("Edit"));
    // A draft of its own: not repeating, so neither Pause nor Resume.
    expect(within(bar("campaigns")).getByText("Draft")).toBeTruthy();
    expect(barActs("campaigns")).not.toContain("Pause");
    expect(barActs("campaigns")).not.toContain("Resume");
    expect(within(pane("campaigns")).queryByText(/Every 3 weeks/)).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// One campaign page
// ---------------------------------------------------------------------------

describe("one campaign page", () => {
  it("asks whether sending is set up once, and hands the answer to every part of the page", async () => {
    const conn = fakeConnection({
      campaigns: [campaignRow({ id: "c1", name: "August update" })],
      templates: [templateRow({ id: "v1:campaigns:template:t1" })],
      sendingReadiness: [{ ready: false, reason: "Verify the sending domain first.", capture: false }],
    });
    mount(conn);
    fireEvent.click(await within(pane("campaigns")).findByText("August update"));
    // The bar, the page and the test all say so -- from the one answer.
    await waitFor(() => expect(within(bar("campaigns")).getByText("Sending isn't set up for Operator organization.")).toBeTruthy());
    expect(within(pane("campaigns")).getByText("Verify the sending domain first.")).toBeTruthy();
    expect(barActs("campaigns")).not.toContain("Send a test");
    await act(async () => {});
    expect(conn.query.campaignSendingReadiness).toHaveBeenCalledTimes(1);
    expect(conn.query.campaignSendingReadiness).toHaveBeenCalledWith({ campaignId: "c1" }, expect.anything());
  });

  it("offers repeating to a draft, on its Edit page, and to nothing else", async () => {
    const conn = fakeConnection({
      campaigns: [
        campaignRow({ id: "d1", name: "Draft one" }),
        campaignRow({ id: "s1", name: "Already sent", status: "sent" }),
      ],
    });
    const { view } = mount(conn, "campaigns", { includeArchived: true });
    fireEvent.click(await within(pane("campaigns")).findByText("Draft one"));
    await waitFor(() => expect(barActs("campaigns")).toContain("Edit"));
    fireEvent.click(within(bar("campaigns")).getByRole("button", { name: "Edit" }));
    const when = await within(pane("campaigns")).findByLabelText("When");
    fireEvent.click(when);
    expect(within(document.getElementById(when.getAttribute("aria-controls")!)!).getByRole("option", { name: "Repeat" })).toBeTruthy();

    view.unmount();
    mount(conn, "campaigns", { includeArchived: true });
    fireEvent.click(await within(pane("campaigns")).findByText("Already sent"));
    await waitFor(() => expect(barActs("campaigns")).toEqual(["Send a test", "Duplicate"]));
    expect(barActs("campaigns")).not.toContain("Edit");
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

  it("says so, in the engine's sentence, when it could not read every id -- and still holds the ids it could", async () => {
    const conn = fakeConnection({ testSettings: [{ id: "ts1", accountId: "acme", audienceId: "a1" }] });
    conn.query.campaignTestSettings.mockImplementation(async (args) => {
      if ((args as { accountId?: string }).accountId === "other") throw new Error("Reading testing settings is not yours to do.");
      return rowsResult([{ id: "ts1", accountId: "acme", audienceId: "a1" }]);
    });
    h.connection = conn;
    const { result } = renderHook(() => useTestSettingsFeed(["acme", "other"]));
    // The failure is the feed's to report, and it is not a zero...
    await waitFor(() => expect(result.current.snapshot.error).toBe("Reading testing settings is not yours to do."));
    // ...but it is the REFUSED id's: one organization this person may not read
    // does not empty the feed for the one they may.
    expect(result.current.snapshot.rows).toEqual([{ id: "ts1", accountId: "acme", audienceId: "a1" }]);
    expect(result.current.refusal("other")).toBe("Reading testing settings is not yours to do.");
    expect(result.current.refusal("acme")).toBe("");
    // Both were read; one was answered and one was refused. An id nobody asked about was neither.
    expect(result.current.settled("acme")).toBe(true);
    expect(result.current.settled("other")).toBe(true);
    expect(result.current.settled("never-asked")).toBe(false);
  });

  it("does not call an id read until it has been", async () => {
    const conn = fakeConnection({ testSettings: [{ id: "ts1", accountId: "acme", audienceId: "a1" }] });
    let release!: () => void;
    const gate = new Promise<void>((done) => { release = done; });
    const answer = conn.query.campaignTestSettings.getMockImplementation()!;
    conn.query.campaignTestSettings.mockImplementation(async (args, opts) => { await gate; return answer(args, opts); });
    h.connection = conn;
    const { result } = renderHook(() => useTestSettingsFeed(["acme"]));

    // No row yet is "not read yet", which is not "no testing audience".
    expect(result.current.settled("acme")).toBe(false);
    await act(async () => { release(); });
    await waitFor(() => expect(result.current.settled("acme")).toBe(true));
    expect(result.current.snapshot.rows).toHaveLength(1);
  });

  it("reads one id again and leaves every other id as it was", async () => {
    const acme = { id: "ts1", accountId: "acme", audienceId: "a1", createdAt: "2026-09-01T00:00:00Z" };
    const other = { id: "ts2", accountId: "other", audienceId: "a2", createdAt: "2026-09-02T00:00:00Z" };
    const conn = fakeConnection({ testSettings: [acme, other] });
    h.connection = conn;
    const { result } = renderHook(() => useTestSettingsFeed(["acme", "other"]));
    await waitFor(() => expect(result.current.snapshot.rows).toHaveLength(2));
    const reads = conn.query.campaignTestSettings.mock.calls.length;
    expect(reads).toBe(2);

    conn.query.campaignTestSettings.mockImplementation(async (args) =>
      rowsResult((args as { accountId?: string }).accountId === "acme" ? [{ ...acme, audienceId: "a9", createdAt: "2026-10-01T00:00:00Z" }] : [other]),
    );
    act(() => result.current.refresh("acme"));
    await waitFor(() => expect(result.current.snapshot.rows.find((row) => row.accountId === "acme")).toMatchObject({ audienceId: "a9" }));

    // One more read, for the id that changed; the other organization is neither read nor lost.
    expect(conn.query.campaignTestSettings).toHaveBeenCalledTimes(reads + 1);
    expect(conn.query.campaignTestSettings.mock.calls.at(-1)![0]).toEqual({ accountId: "acme" });
    expect(result.current.snapshot.rows.find((row) => row.accountId === "other")).toEqual(other);
    expect(opened(conn, TEST_SETTINGS)).toBe(1);
  });

  it("keeps an id's last rows, and says why, when a later read of it is refused", async () => {
    const acme = { id: "ts1", accountId: "acme", audienceId: "a1", createdAt: "2026-09-01T00:00:00Z" };
    const conn = fakeConnection({ testSettings: [acme] });
    h.connection = conn;
    const { result } = renderHook(() => useTestSettingsFeed(["acme"]));
    await waitFor(() => expect(result.current.snapshot.rows).toEqual([acme]));

    conn.query.campaignTestSettings.mockRejectedValueOnce(new Error("The cluster is busy."));
    act(() => result.current.refresh("acme"));
    await waitFor(() => expect(result.current.refusal("acme")).toBe("The cluster is busy."));
    // What was known stays known: a refusal is not a zero.
    expect(result.current.snapshot.rows).toEqual([acme]);

    act(() => result.current.refresh("acme"));
    await waitFor(() => expect(result.current.refusal("acme")).toBe(""));
    expect(result.current.snapshot.error).toBe("");
  });

  it("still reads an id that joined the set in the same commit as a single-id refresh", async () => {
    // A refresh that lands between the render that adds an id and the effect that
    // reads it must not mark the whole set as read: the new id would never be asked.
    const acme = { id: "ts1", accountId: "acme", audienceId: "a1", createdAt: "2026-09-01T00:00:00Z" };
    const other = { id: "ts2", accountId: "other", audienceId: "a2", createdAt: "2026-09-02T00:00:00Z" };
    const conn = fakeConnection({ testSettings: [acme, other] });
    h.connection = conn;
    const { result, rerender } = renderHook(
      ({ ids }) => {
        const feed = useTestSettingsFeed(ids);
        // Layout effects run before the feed's own passive one: the refresh is first.
        useLayoutEffect(() => { if (ids.includes("other")) feed.refresh("acme"); }, [ids.length]);
        return feed;
      },
      { initialProps: { ids: ["acme"] } },
    );
    await waitFor(() => expect(result.current.settled("acme")).toBe(true));

    rerender({ ids: ["acme", "other"] });
    await waitFor(() => expect(result.current.settled("other")).toBe(true));
    expect(result.current.snapshot.rows.map((row) => row.accountId).sort()).toEqual(["acme", "other"]);
  });

  it("reads everything again when a single-id read is asked for while a read is already running", async () => {
    const conn = fakeConnection({ testSettings: [{ id: "ts1", accountId: "acme", audienceId: "a1" }, { id: "ts2", accountId: "other", audienceId: "a2" }] });
    let release!: () => void;
    const gate = new Promise<void>((done) => { release = done; });
    const answer = conn.query.campaignTestSettings.getMockImplementation()!;
    conn.query.campaignTestSettings.mockImplementation(async (args, opts) => { await gate; return answer(args, opts); });
    h.connection = conn;
    const { result } = renderHook(() => useTestSettingsFeed(["acme", "other"]));
    await waitFor(() => expect(conn.query.campaignTestSettings).toHaveBeenCalledTimes(2));

    // The running read may be a recovery from a gap in the stream, which a
    // single-id read must never stand in for: the follow-up is a whole read.
    act(() => result.current.refresh("acme"));
    await act(async () => { release(); });
    await waitFor(() => expect(conn.query.campaignTestSettings).toHaveBeenCalledTimes(4));
    expect(conn.query.campaignTestSettings.mock.calls.slice(2).map(([args]) => (args as { accountId: string }).accountId).sort()).toEqual(["acme", "other"]);
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
// What a feed knows
// ---------------------------------------------------------------------------

describe("what a surface may say about a feed", () => {
  type Stoppable = { markDisconnected: () => void };

  it("reads, answers, refuses, and falls behind: four different things, never a warning for the first", async () => {
    const conn = fakeConnection({ testSettings: [{ id: "ts1", accountId: "acme", audienceId: "a1" }] });
    let release!: () => void;
    const gate = new Promise<void>((done) => { release = done; });
    const answer = conn.query.campaignTestSettings.getMockImplementation()!;
    conn.query.campaignTestSettings.mockImplementation(async (args, opts) => {
      await gate;
      if ((args as { accountId?: string }).accountId === "other") throw new Error("Not yours to read.");
      return answer(args, opts);
    });
    h.connection = conn;
    const { result } = renderHook(() => useTestSettingsFeed(["acme", "other"]));

    expect(knowledgeOf(result.current, "acme")).toEqual({ knowledge: "reading", refusal: "" });
    await act(async () => { release(); });
    await waitFor(() => expect(knowledgeOf(result.current, "acme").knowledge).toBe("known"));
    expect(knowledgeOf(result.current, "other")).toEqual({ knowledge: "refused", refusal: "Not yours to read." });

    // A feed that has fallen behind cannot confirm what it holds -- the last
    // rows stay, and the surface says so; a refusal is still the engine's own.
    act(() => (result.current.source as unknown as Stoppable).markDisconnected());
    expect(knowledgeOf(result.current, "acme")).toEqual({ knowledge: "unconfirmed", refusal: "" });
    expect(knowledgeOf(result.current, "other").knowledge).toBe("refused");
    expect(result.current.snapshot.rows).toHaveLength(1);
  });

  it("knows a list feed is reading until it is not, and behind when it is", async () => {
    const conn = fakeConnection({ audiences: [audienceRow({ id: "a1" })] });
    let release!: () => void;
    const gate = new Promise<void>((done) => { release = done; });
    const answer = conn.query.audiences.getMockImplementation()!;
    conn.query.audiences.mockImplementation(async (args, opts) => { await gate; return answer(args, opts); });
    h.connection = conn;
    const { result } = renderHook(() => useEmailFeeds());
    expect(listKnowledge(result.current.audiences)).toBe("reading");
    await act(async () => { release(); });
    await waitFor(() => expect(listKnowledge(result.current.audiences)).toBe("known"));
    act(() => (result.current.audiences.source as unknown as Stoppable).markDisconnected());
    expect(listKnowledge(result.current.audiences)).toBe("unconfirmed");
  });

  it("claims no more than the least of several answers: refused, then behind, then reading", () => {
    expect(weakest("known", "known")).toBe("known");
    expect(weakest("known", "reading")).toBe("reading");
    expect(weakest("reading", "unconfirmed")).toBe("unconfirmed");
    expect(weakest("unconfirmed", "refused", "known")).toBe("refused");
    expect(weakest()).toBe("known");
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

  // The Add a campaign wizard shows a count beside every audience it offers,
  // and an audience added in another tab arrives while it is open: the counts it
  // already has must not go back to skeletons while the new one is read.
  it("keeps the counts it has while an audience that arrived is counted", async () => {
    const conn = fakeConnection({ subscriberCounts: { a1: 7, a2: 3 } });
    h.connection = conn;
    const { result, rerender } = renderHook(({ ids }) => useSubscriberCounts(ids), { initialProps: { ids: ["a1"] } });
    await waitFor(() => expect(result.current.counts.get("a1")).toEqual(figureOf(7)));

    const release = deferred(conn);
    rerender({ ids: ["a1", "a2"] });
    expect(result.current.counts.get("a1")).toEqual(figureOf(7));
    expect(result.current.counts.get("a2")).toEqual(absent("unread"));

    await act(async () => { release(); });
    await waitFor(() => expect(result.current.counts.get("a2")).toEqual(figureOf(3)));
    expect(result.current.counts.get("a1")).toEqual(figureOf(7));
  });

  it("counts a few audiences at a time, however many there are", async () => {
    const conn = fakeConnection();
    let inFlight = 0;
    let most = 0;
    conn.query.audienceSubscriberCount.mockImplementation(async () => {
      inFlight += 1;
      most = Math.max(most, inFlight);
      await new Promise((settle) => setTimeout(settle, 1));
      inFlight -= 1;
      return rowsResult([{ count: 1 }]);
    });
    h.connection = conn;
    const ids = Array.from({ length: 20 }, (_, n) => `a${n}`);
    const { result } = renderHook(() => useSubscriberCounts(ids));
    await waitFor(() => expect(ids.every((id) => result.current.counts.get(id)?.kind === "measured")).toBe(true));
    expect(conn.query.audienceSubscriberCount).toHaveBeenCalledTimes(20);
    expect(most).toBeLessThanOrEqual(6);
  });
});
