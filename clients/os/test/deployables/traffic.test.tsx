import { render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection is a module-level context read and its provider dials a real
// websocket, so the hook is replaced rather than the provider mounted -- the
// shape deployables.test.tsx already uses.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import {
  DEFAULT_TRAFFIC_WINDOW,
  LIST_TRAFFIC_WINDOW,
  MAX_SITES_PER_READ,
  fetchTrafficSummaries,
  readingFromRows,
  seriesSummary,
  unmeasuredSentence,
  windowBounds,
  windowLabel,
  windowSpec,
  type TrafficWindow,
} from "../../src/apps/deployables/traffic";
import {
  SETTINGS_KEY_FORM,
  settingsFingerprint,
  settingsKeyProblem,
  settingsRows,
  toSettingsMap,
} from "../../src/apps/deployables/settings-editor";
import {
  PORTAL,
  SHOP,
  click,
  fakeConnection,
  siteRow,
  trafficRow,
  type as typeInto,
  withSession,
  type FakeConnection,
  type FakeSeed,
} from "./harness";

// The traffic figure and the runtime-settings editor on the Live stop
// (epic memql#4906).
//
// The pure halves are asserted directly, because what a reading MEANS -- an
// absent row versus a zero, an aligned window, a gap inside a measured window
// -- is a claim about functions. The rendered half asserts the SENTENCES,
// because the words are the design: "nothing was recorded" and "0 errors" are
// two different answers and a person has to be able to tell them apart.

// ---------------------------------------------------------------------------
// The reading
// ---------------------------------------------------------------------------

describe("the traffic reading", () => {
  const day: TrafficWindow = "day";

  it("answers null for a window the server measured nothing in", () => {
    const bounds = windowBounds(day, new Date("2026-09-03T12:34:00Z"));
    expect(readingFromRows([], day, bounds)).toBeNull();
  });

  it("answers a reading with zero errors for a window that had requests and no errors", () => {
    const now = new Date("2026-09-03T12:34:00Z");
    const bounds = windowBounds(day, now);
    const reading = readingFromRows(
      [trafficRow({ windowStart: bounds.start, requestCount: 12, lastServedAt: bounds.start })],
      day,
      bounds,
    );
    expect(reading).not.toBeNull();
    expect(reading?.requests).toBe(12);
    // THE DISTINCTION THE WHOLE FEATURE TURNS ON: this is a zero, and the
    // case above is an absence. They must not fold into one another.
    expect(reading?.errors).toBe(0);
    expect(reading?.notFound).toBe(0);
  });

  it("fills gaps inside a measured window with zeroes rather than closing up", () => {
    const now = new Date("2026-09-03T12:00:00Z");
    const bounds = windowBounds(day, now);
    const start = Date.parse(bounds.start);
    const spec = windowSpec(day);
    const reading = readingFromRows(
      [
        trafficRow({ windowStart: new Date(start).toISOString(), requestCount: 5 }),
        trafficRow({ windowStart: new Date(start + 3 * spec.bucketMs).toISOString(), requestCount: 7 }),
      ],
      day,
      bounds,
    );
    expect(reading?.requests).toBe(12);
    // A day of hour buckets is twenty-four columns whatever arrived: a quiet
    // night has to LOOK quiet, and a series that closed up its gaps would
    // draw two busy hours as continuous traffic.
    expect(reading?.buckets).toHaveLength(24);
    expect(reading?.buckets[0]?.requests).toBe(5);
    expect(reading?.buckets[1]?.requests).toBe(0);
    expect(reading?.buckets[3]?.requests).toBe(7);
  });

  it("sums every bucket's counts and keeps the newest lastServedAt", () => {
    const now = new Date("2026-09-03T12:00:00Z");
    const bounds = windowBounds(day, now);
    const start = Date.parse(bounds.start);
    const spec = windowSpec(day);
    const reading = readingFromRows(
      [
        trafficRow({ windowStart: new Date(start).toISOString(), requestCount: 5, errorCount: 1, clientErrorCount: 2, lastServedAt: "2026-09-02T13:00:00Z" }),
        trafficRow({ windowStart: new Date(start + spec.bucketMs).toISOString(), requestCount: 7, errorCount: 3, lastServedAt: "2026-09-03T11:59:00Z" }),
      ],
      day,
      bounds,
    );
    expect(reading?.requests).toBe(12);
    expect(reading?.errors).toBe(4);
    expect(reading?.notFound).toBe(2);
    expect(reading?.lastServedAt).toBe("2026-09-03T11:59:00Z");
  });

  it("keeps the totals equal to the sum of the strip", () => {
    const now = new Date("2026-09-03T12:00:00Z");
    const bounds = windowBounds(day, now);
    const start = Date.parse(bounds.start);
    const reading = readingFromRows(
      [
        trafficRow({ windowStart: new Date(start).toISOString(), requestCount: 5, errorCount: 1 }),
        // Off the grid and outside the window: contributes to NEITHER, so the
        // picture and the numbers beneath it cannot disagree. "The total says
        // 1,330 and the columns are empty" is a state a reader cannot resolve.
        trafficRow({ windowStart: new Date(start - 99_000_000).toISOString(), requestCount: 900 }),
        trafficRow({ windowStart: "not-a-date", requestCount: 900 }),
      ],
      day,
      bounds,
    );
    expect(reading?.requests).toBe(5);
    expect(reading?.buckets.reduce((n, b) => n + b.requests, 0)).toBe(reading?.requests);
    expect(reading?.buckets.reduce((n, b) => n + b.errors, 0)).toBe(reading?.errors);
  });

  it("aligns each window to its own bucket, half-open", () => {
    const now = new Date("2026-09-03T12:34:56.789Z");
    for (const window of ["hour", "day", "week"] as const) {
      const spec = windowSpec(window);
      const { start, end } = windowBounds(window, now);
      // ALIGNED BOTH ENDS: the server selects on bucket boundaries, so an
      // unaligned bound drops the bucket it lands inside.
      expect(Date.parse(start) % spec.bucketMs).toBe(0);
      expect(Date.parse(end) % spec.bucketMs).toBe(0);
      expect(Date.parse(end) - Date.parse(start)).toBe(spec.spanMs);
      // The end is aligned UP, so the bucket in progress is included -- which
      // is what lets "last served" say seconds ago rather than an hour ago.
      expect(Date.parse(end)).toBeGreaterThanOrEqual(now.getTime());
    }
  });

  it("reads an hour in minute buckets and a week in hour buckets", () => {
    expect(windowSpec("hour").bucket).toBe("1m");
    expect(windowSpec("day").bucket).toBe("1h");
    // A week of MINUTE buckets would be ten thousand rows to draw one line.
    expect(windowSpec("week").bucket).toBe("1h");
  });

  it("refreshes on a cadence that widens with the window, and never faster than a minute", () => {
    const specs = (["hour", "day", "week"] as const).map(windowSpec);
    for (const spec of specs) {
      // Never a hot loop: the figure is a read against the database, and a
      // surface left open on a desk would make one per second forever.
      expect(spec.refreshMs).toBeGreaterThanOrEqual(60_000);
    }
    // A WIDER WINDOW MOVES MORE SLOWLY. One more request changes an hour's
    // figure visibly and a week's not at all, so the cadence follows the
    // window rather than the bucket.
    expect(specs[0]!.refreshMs).toBeLessThan(specs[1]!.refreshMs);
    expect(specs[1]!.refreshMs).toBeLessThan(specs[2]!.refreshMs);
  });

  it("names both causes when a window is unmeasured", () => {
    const sentence = unmeasuredSentence("day");
    // Somebody looking at their own app cannot tell these apart, and the
    // wrong guess is expensive: one is a business fact, one is a cluster fact.
    expect(sentence).toContain("nobody visited");
    expect(sentence).toContain("not recording");
  });

  it("summarizes the series in words for a reader who cannot see it", () => {
    const now = new Date("2026-09-03T12:00:00Z");
    const bounds = windowBounds("day", now);
    const start = Date.parse(bounds.start);
    const reading = readingFromRows(
      [trafficRow({ windowStart: new Date(start).toISOString(), requestCount: 9 })],
      "day",
      bounds,
    )!;
    const summary = seriesSummary(reading, "day");
    expect(summary).toContain("9 requests");
    expect(summary).toContain("24 hour buckets");
    expect(summary).toContain("23 were empty");
  });

  it("pages the list read at the server's own cap", async () => {
    // The server REFUSES a call past its cap rather than truncating it, so a
    // cluster with more deployables than that must produce several calls --
    // one refusal would leave the whole list with no figures, which reads as
    // "nobody is using any of these".
    const ids = Array.from({ length: MAX_SITES_PER_READ + 3 }, (_, i) => `site-${i}`);
    const calls: string[] = [];
    const query = {
      siteTrafficInWindow: async (args: { siteIds: string[] }) => {
        calls.push(args.siteIds.join(","));
        return { rows: () => [] };
      },
    } as unknown as Parameters<typeof fetchTrafficSummaries>[0];

    await fetchTrafficSummaries(query, ids, "week", new Date("2026-09-03T12:34:00Z"));
    expect(calls).toHaveLength(2);
    expect(calls[0]!.split(",")).toHaveLength(MAX_SITES_PER_READ);
    expect(calls[1]!.split(",")).toHaveLength(3);
    // Every id is asked about exactly once: a page boundary that dropped or
    // repeated one would be invisible in the rendered list.
    expect(new Set(calls.join(",").split(","))).toEqual(new Set(ids));
  });

  it("reads a WIDER window for the list than for the stop", () => {
    // The stop asks "how is it doing"; a list row asks "is anybody using this
    // at all", and a day-wide row would call a weekly app abandoned.
    expect(DEFAULT_TRAFFIC_WINDOW).toBe("day");
    expect(LIST_TRAFFIC_WINDOW).toBe("week");
  });
});

// ---------------------------------------------------------------------------
// The settings editor's arithmetic
// ---------------------------------------------------------------------------

describe("the runtime-settings editor", () => {
  it("mirrors the server's key form", () => {
    for (const ok of ["apiBase", "A", "a_b_c9", "k".padEnd(64, "9")]) {
      expect(SETTINGS_KEY_FORM.test(ok)).toBe(true);
    }
    for (const bad of ["", "9lives", "_lead", "api-base", "api.base", "k".padEnd(65, "9")]) {
      expect(SETTINGS_KEY_FORM.test(bad)).toBe(false);
    }
  });

  it("orders rows by key so a save does not reshuffle them under the person who made it", () => {
    expect(settingsRows({ zulu: "1", alpha: "2" }).map((r) => r.key)).toEqual(["alpha", "zulu"]);
  });

  it("drops a blank key from the save and keeps a blank value", () => {
    const rows = [
      { id: "1", key: "apiBase", value: "https://api.example" },
      { id: "2", key: "", value: "orphan" },
      { id: "3", key: "region", value: "" },
    ];
    // A row somebody has just added must not make the whole save refuse; an
    // empty string IS a value a bundle can read.
    expect(toSettingsMap(rows)).toEqual({ apiBase: "https://api.example", region: "" });
  });

  it("says nothing about a key nobody has typed yet", () => {
    expect(settingsKeyProblem("", [])).toBe("");
  });

  it("names the form when a key cannot be read by an app", () => {
    expect(settingsKeyProblem("api-base", [{ id: "1", key: "api-base", value: "" }])).toContain("config.settings");
  });

  it("compares two settings objects by their VALUES, not by key order", () => {
    // The stored map arrives from the wire in the payload's order and the
    // draft is rebuilt from rows sorted by key, so a raw stringify comparison
    // reports a difference on a form nobody has touched -- a Save button lit
    // on an untouched panel, and an edit that looks unsaved after it saved.
    const wireOrder = { zulu: "1", alpha: "2" };
    const draftOrder = { alpha: "2", zulu: "1" };
    expect(JSON.stringify(wireOrder)).not.toBe(JSON.stringify(draftOrder));
    expect(settingsFingerprint(wireOrder)).toBe(settingsFingerprint(draftOrder));
    // And it still tells a real change apart.
    expect(settingsFingerprint(wireOrder)).not.toBe(settingsFingerprint({ alpha: "2", zulu: "changed" }));
    expect(settingsFingerprint(wireOrder)).not.toBe(settingsFingerprint({ alpha: "2" }));
  });

  it("catches two rows sharing a name, which the server structurally cannot", () => {
    const rows = [
      { id: "1", key: "apiBase", value: "a" },
      { id: "2", key: "apiBase", value: "b" },
    ];
    // The two collapse into one object key on the way out, so the save would
    // succeed and quietly keep whichever came last.
    expect(settingsKeyProblem("apiBase", rows)).toContain("Only one of them would be saved");
    expect(Object.keys(toSettingsMap(rows))).toHaveLength(1);
  });
});

// ---------------------------------------------------------------------------
// On the page
// ---------------------------------------------------------------------------

function mount(connection: FakeConnection) {
  h.connection = connection;
  return render(
    withSession(<DeployablesApp sectionId="deployables" navigate={vi.fn()} askContext={vi.fn()} />, {
      role: "owner",
      userId: "u-me",
    }),
  );
}

/**
 * Open a deployable and return its PAGE.
 *
 * Scoped to the page region rather than to the document, and that is the
 * lesson from the section growing a map beside its list: a hostname matches
 * the list row AND the page head AND the rail's address, so a
 * document-wide query for one finds several. Every assertion below reads
 * inside the region the page owns.
 */
async function openDeployable(seed: FakeSeed, hostname: string): Promise<{ connection: FakeConnection; page: HTMLElement }> {
  const connection = fakeConnection(seed);
  mount(connection);
  await waitFor(() =>
    expect(document.querySelector("[data-os-livelist]")?.getAttribute("data-state")).toBe("live"),
  );
  // Before the page opens the only element carrying the hostname is the list
  // row; once it is open the head and the rail do too.
  await click((await screen.findByText(hostname)).closest("button"));
  const page = await screen.findByRole("region", { name: `Deployable ${hostname}` });
  return { connection, page };
}

describe("a deployable's readings, rendered", () => {
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    // NOT ON A BUCKET BOUNDARY. `windowBounds` aligns the end UP, so at
    // exactly 12:00:00.000 a fixture built here and the component's own read
    // a few milliseconds later land on windows an hour apart -- the fixture's
    // rows then fall outside the component's window and the strip renders
    // empty, which reads as a projection bug rather than as a test that
    // picked a fragile instant.
    vi.setSystemTime(new Date("2026-09-03T12:34:00Z"));
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  const SHOP_LIVE = siteRow({ ...SHOP, id: "site-shop", status: "live" });

  it("says a window was unmeasured in words rather than showing zeroes", async () => {
    const { page } = await openDeployable({ sites: [SHOP_LIVE] }, "shop.memql.example.com");
    await waitFor(() => {
      expect(within(page).getByText(unmeasuredSentence(DEFAULT_TRAFFIC_WINDOW))).toBeTruthy();
    });
    // NO FIGURE AT ALL, rather than a row of zeroes: an absent figure and a
    // zero are different answers.
    expect(within(page).queryByText("Requests")).toBeNull();
  });

  it("states zero errors when the window had requests and none failed", async () => {
    const bounds = windowBounds("day", new Date("2026-09-03T12:34:00Z"));
    const { page } = await openDeployable(
      {
        sites: [SHOP_LIVE],
        traffic: { series: [trafficRow({ windowStart: bounds.start, requestCount: 42, lastServedAt: "2026-09-03T11:58:00Z" })] },
      },
      "shop.memql.example.com",
    );
    await waitFor(() => expect(within(page).getByText("Requests")).toBeTruthy());
    const facts = within(page).getByText("Requests").closest("dl")!;
    expect(within(facts).getByText("42")).toBeTruthy();
    // Zero, stated. Not an em dash and not an absence.
    expect(within(facts).getByText("Errors").nextElementSibling?.textContent).toBe("0");
    expect(within(facts).getByText("Not found").nextElementSibling?.textContent).toBe("0");
  });

  it("draws one column per bucket, with the empty ones drawn empty", async () => {
    const bounds = windowBounds("day", new Date("2026-09-03T12:34:00Z"));
    const start = Date.parse(bounds.start);
    const { page } = await openDeployable(
      {
        sites: [SHOP_LIVE],
        traffic: {
          series: [
            trafficRow({ windowStart: new Date(start).toISOString(), requestCount: 10 }),
            trafficRow({ windowStart: new Date(start + 3600_000).toISOString(), requestCount: 4, errorCount: 2 }),
          ],
        },
      },
      "shop.memql.example.com",
    );
    const strip = await within(page).findByRole("list", { name: /requests per hour/i });
    const columns = within(strip).getAllByRole("listitem");
    expect(columns).toHaveLength(24);
    // The error band is inked on the bucket that had errors and nowhere else,
    // and it is a SECOND carrier beside the Errors fact -- never colour alone.
    expect(columns.filter((c) => c.getAttribute("data-errors") === "true")).toHaveLength(1);
  });

  it("changes the bucket and the read when the window changes", async () => {
    const { connection, page } = await openDeployable({ sites: [SHOP_LIVE] }, "shop.memql.example.com");
    await waitFor(() => expect(connection.callsNamed("siteTrafficInWindow").length).toBeGreaterThan(0));
    const before = connection.callsNamed("siteTrafficInWindow").at(-1)!;
    expect(before).toContain('bucket: "1h"');

    await click(within(page).getByRole("radio", { name: windowLabel("hour") }));
    await waitFor(() => {
      const after = connection.callsNamed("siteTrafficInWindow").at(-1)!;
      expect(after).toContain('bucket: "1m"');
    });
  });

  it("says nothing about a system-owned surface's traffic, and accounts for the absence", async () => {
    const { connection, page } = await openDeployable({ sites: [PORTAL] }, "portal.memql.example.com");
    // ONE note covering every absence on the stop, rather than one per
    // missing control -- and it names the traffic among them, so the missing
    // figure reads as a decision rather than as something unbuilt.
    await waitFor(() => expect(within(page).getByText(/traffic is not recorded/i)).toBeTruthy());
    expect(within(page).queryByRole("radiogroup", { name: /traffic window/i })).toBeNull();
    // AND NOTHING WAS ASKED FOR. The cluster's own surfaces are excluded from
    // the request log by construction, so a read would have been a call whose
    // answer is known in advance.
    expect(connection.callsNamed("siteTrafficInWindow").filter((c) => c.includes("site-portal"))).toHaveLength(0);
  });

  it("reports a refused read in the server's own words", async () => {
    const { page } = await openDeployable(
      { sites: [SHOP_LIVE], trafficError: "a traffic read covers at most 200 deployables at once" },
      "shop.memql.example.com",
    );
    await waitFor(() => {
      expect(within(page).getByText(/a traffic read covers at most 200 deployables/)).toBeTruthy();
    });
  });

  it("shows the settings a deployable carries, and the sentence about secrets", async () => {
    const { page } = await openDeployable(
      { sites: [siteRow({ ...SHOP_LIVE, id: "site-shop", settings: { apiBase: "https://api.eu.example" } })] },
      "shop.memql.example.com",
    );
    await waitFor(() => expect(within(page).getByText(/Not a place for a secret/i)).toBeTruthy());
    expect((within(page).getAllByRole("textbox") as HTMLInputElement[]).some((i) => i.value === "apiBase")).toBe(true);
    expect((within(page).getAllByRole("textbox") as HTMLInputElement[]).some((i) => i.value === "https://api.eu.example")).toBe(true);
  });

  it("sends the whole map, so removing a setting is expressible", async () => {
    const { connection, page } = await openDeployable(
      { sites: [siteRow({ ...SHOP_LIVE, id: "site-shop", settings: { apiBase: "https://api.eu.example", region: "eu" } })] },
      "shop.memql.example.com",
    );
    await waitFor(() => expect(within(page).getAllByRole("button", { name: /^Remove region$/ }).length).toBe(1));
    await click(within(page).getByRole("button", { name: /^Remove region$/ }));
    await click(within(page).getByRole("button", { name: /save settings/i }));

    await waitFor(() => expect(connection.callsNamed("updateSiteSettings").length).toBe(1));
    const call = connection.callsNamed("updateSiteSettings")[0]!;
    expect(call).toContain("apiBase");
    // THE WHOLE MAP: a merge would have kept `region` and made the removal
    // silently do nothing.
    expect(call).not.toContain("region");
  });

  it("reports a refused save verbatim", async () => {
    const { page } = await openDeployable(
      {
        sites: [siteRow({ ...SHOP_LIVE, id: "site-shop", settings: { apiBase: "x" } })],
        settingsError:
          'v1:platform:site: settings key "apiTokenRef" ends in Ref, and a setting is never a reference',
      },
      "shop.memql.example.com",
    );
    const value = (within(page).getAllByRole("textbox") as HTMLInputElement[]).find((i) => i.value === "x")!;
    await typeInto(value, "y");
    await click(within(page).getByRole("button", { name: /save settings/i }));
    await waitFor(() => expect(within(page).getByText(/ends in Ref/)).toBeTruthy());
  });

  it("renders no settings controls at all on a system-owned row", async () => {
    const { page } = await openDeployable({ sites: [PORTAL] }, "portal.memql.example.com");
    await waitFor(() => expect(within(page).getByText(/traffic is not recorded/i)).toBeTruthy());
    expect(within(page).queryByRole("button", { name: /add a setting/i })).toBeNull();
    expect(within(page).queryByRole("button", { name: /save settings/i })).toBeNull();
  });
});

describe("the list row's figure", () => {
  it("agrees with the stop for the same deployable, and says nothing when unmeasured", async () => {
    const summary = trafficRow({
      windowStart: "2026-08-27T12:00:00Z",
      siteId: "site-shop",
      requestCount: 128,
      lastServedAt: "2026-09-03T11:00:00Z",
    });
    const connection = fakeConnection({
      sites: [siteRow({ ...SHOP, id: "site-shop", status: "live" }), siteRow({ id: "site-quiet", hostname: "quiet.memql.example.com" })],
      traffic: { summary: [summary] },
    });
    mount(connection);

    // Scoped to the LIST rather than to a page: nothing is open here, and the
    // map beside the list draws the same deployables.
    const list = await screen.findByRole("list", { name: /deployables/i });
    await waitFor(() => expect(within(list).getByText(/^served /)).toBeTruthy());
    // ONE CALL for the whole list, in the summary mode built for it.
    const calls = connection.callsNamed("siteTrafficInWindow");
    expect(calls.filter((c) => c.includes("summary: true"))).toHaveLength(1);
    expect(calls[0]).toContain('bucket: "1h"');

    // The deployable the answer did not cover is unmeasured, and its row says
    // NOTHING rather than "never" -- which would be a claim.
    expect(within(list).getAllByText(/^served /)).toHaveLength(1);
  });
});
