import { describe, expect, it } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  NO_TRACKED_CAMPAIGN,
  campaignDistribution,
  overviewMetrics,
  startedInWindow,
  statsWanted,
  type OverviewInputs,
} from "../../../src/apps/email/overview/figures";
import { campaignFromRow, formatRate, statsFromPayload, type CampaignStats } from "../../../src/apps/email/rows";
import type { Reading } from "../../../src/apps/email/useEmail";
import { absent, figureOf } from "../../../src/kit/measure";
import { campaignRow } from "../harness";

// WHAT THE OVERVIEW MEASURES, AS PURE FUNCTIONS (memql#6033, Ruling 22).
//
// Five figures and a distribution are functions of what the app's reads have
// answered, so "absent is never zero" and "a rate is of what was sent" are
// properties of a function, asserted with no DOM. A figure is one of three
// things -- a number, a reason there is none, or a read still on its way -- and
// each test below pins which.

const NOW = new Date("2026-10-12T12:00:00Z");
const daysAgo = (days: number) => new Date(NOW.getTime() - days * 86_400_000).toISOString();

/** A campaign as the list projects it. */
function campaign(over: Partial<Row> & { id: string }) {
  return campaignFromRow(campaignRow(over));
}

/** A campaign that went out `days` ago, having sent `sent`. */
function sentCampaign(id: string, days: number, sent: number, over: Partial<Row> = {}) {
  return campaign({ id, status: "sent", startedAt: daysAgo(days), recipientCount: sent, sentCount: sent, ...over });
}

const reload = async () => {};

/** What `campaignStats` answered for a campaign. */
function answered(payload: Row): Reading<CampaignStats | null> {
  return { value: statsFromPayload(payload), state: "ready", error: "", readAt: "2026-10-12T12:00:00Z", reload };
}
const waiting: Reading<CampaignStats | null> = { value: null, state: "loading", error: "", readAt: "", reload };
const refused = (error: string): Reading<CampaignStats | null> => ({ value: null, state: "error", error, readAt: "", reload });

function inputs(over: Partial<OverviewInputs> = {}): OverviewInputs {
  return {
    campaigns: { rows: [], settled: true, error: "" },
    stats: new Map(),
    subscribers: new Map(),
    audiencesSettled: true,
    activeAutomations: { count: 0, settled: true, error: "" },
    now: NOW,
    ...over,
  };
}

function metric(i: OverviewInputs, label: string) {
  const found = overviewMetrics(i).find((m) => m.label === label);
  if (found === undefined) throw new Error(`no metric named ${label}`);
  return found;
}

describe("the five figures", () => {
  it("are named exactly, in this order", () => {
    expect(overviewMetrics(inputs()).map((m) => m.label)).toEqual([
      "Sent, 30 days",
      "Open rate",
      "Click rate",
      "Subscribers",
      "Active automations",
    ]);
  });
});

describe("Sent, 30 days", () => {
  it("counts only campaigns started in the last 30 days", () => {
    const rows = [
      sentCampaign("recent", 3, 100),
      sentCampaign("edge", 29, 50),
      sentCampaign("old", 31, 1000),
      // Never started: a draft and a scheduled campaign have sent nothing and have no start.
      campaign({ id: "draft", status: "draft" }),
      campaign({ id: "scheduled", status: "scheduled", scheduledAt: "2026-10-20T16:00:00Z" }),
      // A send in flight counts what it has sent so far.
      campaign({ id: "now", status: "sending", startedAt: daysAgo(0), recipientCount: 400, sentCount: 40 }),
    ];
    const sent = metric(inputs({ campaigns: { rows, settled: true, error: "" } }), "Sent, 30 days");
    expect(sent.figure).toEqual(figureOf(190));
    expect(sent.loading).toBeFalsy();
    expect(sent.format?.(1204)).toBe("1,204");
  });

  it("is a real zero when nothing went out", () => {
    expect(metric(inputs(), "Sent, 30 days").figure).toEqual(figureOf(0));
  });

  it("tells a start inside the window from one outside it, and ignores a start it cannot read", () => {
    expect(startedInWindow(sentCampaign("a", 29.5, 1), NOW)).toBe(true);
    expect(startedInWindow(sentCampaign("b", 30.5, 1), NOW)).toBe(false);
    expect(startedInWindow(campaign({ id: "c", startedAt: "" }), NOW)).toBe(false);
    expect(startedInWindow(campaign({ id: "d", startedAt: "yesterday" }), NOW)).toBe(false);
    // A clock a little ahead of this one wrote the start: still in the window.
    expect(startedInWindow(campaign({ id: "e", startedAt: new Date(NOW.getTime() + 60_000).toISOString() }), NOW)).toBe(true);
  });

  it("shows its shape while the campaigns are read, and the sentence when they are refused", () => {
    const reading = metric(inputs({ campaigns: { rows: [], settled: false, error: "" } }), "Sent, 30 days");
    expect(reading.loading).toBe(true);
    expect(reading.figure.kind).toBe("absent");

    const failed = metric(inputs({ campaigns: { rows: [], settled: true, error: "Campaigns are not yours to read." } }), "Sent, 30 days");
    expect(failed.loading).toBeFalsy();
    expect(failed.figure).toEqual(absent("failed", "Campaigns are not yours to read."));
  });
});

describe("Open rate and Click rate", () => {
  const A = sentCampaign("a", 2, 1000);
  const B = sentCampaign("b", 5, 500);

  it("a rate is of sent, over campaigns that tracked it", () => {
    // B counted opens and not clicks; C sent ten thousand and counted neither.
    const opensOnly = sentCampaign("b", 5, 500, { trackClicks: false });
    const untracked = sentCampaign("c", 6, 10_000, { trackOpens: false, trackClicks: false });
    const i = inputs({
      campaigns: { rows: [A, opensOnly, untracked], settled: true, error: "" },
      stats: new Map([
        ["a", answered({ sent: 1000, opens: { unique: 300, total: 400 }, clicks: { unique: 50, total: 60 } })],
        // Whatever B's clicks were, nobody asked for them to be counted.
        ["b", answered({ sent: 500, opens: { unique: 100, total: 150 }, clicks: { unique: 400, total: 400 } })],
        ["c", answered({ sent: 10_000, opens: { unique: 9_999, total: 9_999 }, clicks: { unique: 9_999, total: 9_999 } })],
      ]),
    });

    const opens = metric(i, "Open rate");
    expect(opens.figure.kind).toBe("measured");
    // (300 + 100) of (1000 + 500) sent, not of the 11,500 sent in all.
    expect(opens.figure.kind === "measured" && opens.figure.value).toBeCloseTo(400 / 1500, 10);
    expect(opens.format?.(400 / 1500)).toBe(formatRate(400 / 1500));
    expect(opens.format?.(400 / 1500)).toBe("27%");

    // 50 of A's 1000: B did not count clicks and C counted nothing.
    const clicks = metric(i, "Click rate");
    expect(clicks.figure.kind === "measured" && clicks.figure.value).toBeCloseTo(50 / 1000, 10);
    expect(clicks.format?.(50 / 1000)).toBe("5.0%");
  });

  it("is a rate of delivered, never of the audience", () => {
    // 4,000 were meant to receive it; 1,000 were sent. The rate is over the 1,000.
    const sized = campaign({ id: "s", status: "sent", startedAt: daysAgo(1), recipientCount: 4000, sentCount: 1000 });
    const i = inputs({
      campaigns: { rows: [sized], settled: true, error: "" },
      stats: new Map([["s", answered({ sent: 1000, opens: { unique: 250, total: 300 }, clicks: { unique: 10, total: 10 } })]]),
    });
    const opens = metric(i, "Open rate").figure;
    expect(opens.kind === "measured" && opens.value).toBeCloseTo(0.25, 10);
  });

  it("an unmeasured unique figure makes the rate absent with its reason", () => {
    // B's distinct openers were too many to count exactly; A's were counted.
    // Adding A alone would be a rate for part of the sending that reads as all of it.
    const i = inputs({
      campaigns: { rows: [A, B], settled: true, error: "" },
      stats: new Map([
        ["a", answered({ sent: 1000, opens: { unique: 300, total: 400 }, clicks: { unique: 50, total: 60 } })],
        ["b", answered({ sent: 500, opens: { total: 150 }, clicks: { unique: 20, total: 25 } })],
      ]),
    });

    const opens = metric(i, "Open rate");
    expect(opens.loading).toBeFalsy();
    expect(opens.figure.kind).toBe("absent");
    expect(opens.figure).toMatchObject({
      kind: "absent",
      reason: "unmeasured",
      detail: "Too many recorded events to count distinct recipients exactly, so this is left unstated rather than guessed.",
    });
    // The click figures were all measured, so clicks still have a rate.
    expect(metric(i, "Click rate").figure.kind).toBe("measured");
  });

  it("no tracked campaign is absent, never zero", () => {
    const reason = "No tracked campaign in the last 30 days.";
    expect(NO_TRACKED_CAMPAIGN).toBe(reason);

    // No campaign at all.
    expect(metric(inputs(), "Open rate").figure).toEqual(absent("unmeasured", reason));
    expect(metric(inputs(), "Click rate").figure).toEqual(absent("unmeasured", reason));

    // Campaigns, none of which counted opens or clicks.
    const untracked = [
      sentCampaign("u1", 2, 800, { trackOpens: false, trackClicks: false }),
      sentCampaign("u2", 4, 200, { trackOpens: false, trackClicks: false }),
    ];
    const i = inputs({ campaigns: { rows: untracked, settled: true, error: "" } });
    expect(metric(i, "Open rate").figure).toEqual(absent("unmeasured", reason));
    expect(metric(i, "Click rate").figure).toEqual(absent("unmeasured", reason));

    // A tracked campaign that went out before the window is not in it.
    const old = inputs({ campaigns: { rows: [sentCampaign("o", 40, 900)], settled: true, error: "" } });
    expect(metric(old, "Open rate").figure).toEqual(absent("unmeasured", reason));

    // A tracked campaign that has sent nothing has no rate to report.
    const nothing = inputs({ campaigns: { rows: [campaign({ id: "n", status: "sending", startedAt: daysAgo(0), recipientCount: 10, sentCount: 0 })], settled: true, error: "" } });
    expect(metric(nothing, "Open rate").figure).toEqual(absent("unmeasured", reason));
  });

  it("a measured zero is a zero: it was counted and nobody opened", () => {
    const i = inputs({
      campaigns: { rows: [A], settled: true, error: "" },
      stats: new Map([["a", answered({ sent: 1000, opens: { unique: 0, total: 0 }, clicks: { unique: 0, total: 0 } })]]),
    });
    expect(metric(i, "Open rate").figure).toEqual(figureOf(0));
    expect(metric(i, "Open rate").format?.(0)).toBe("0%");
  });

  it("waits for every campaign's breakdown, and shows its shape meanwhile", () => {
    const i = inputs({
      campaigns: { rows: [A, B], settled: true, error: "" },
      stats: new Map([["a", answered({ sent: 1000, opens: { unique: 300, total: 400 }, clicks: { unique: 50, total: 60 } })], ["b", waiting]]),
    });
    expect(metric(i, "Open rate")).toMatchObject({ loading: true });
    expect(metric(i, "Click rate")).toMatchObject({ loading: true });

    // A breakdown nobody has asked for yet is a breakdown still to come.
    const unasked = inputs({ campaigns: { rows: [A], settled: true, error: "" } });
    expect(metric(unasked, "Open rate")).toMatchObject({ loading: true });
  });

  it("says the engine's sentence when a breakdown is refused, whatever else is still on its way", () => {
    const i = inputs({
      campaigns: { rows: [A, B], settled: true, error: "" },
      stats: new Map([["a", refused("Breakdowns are not yours to read.")], ["b", waiting]]),
    });
    const opens = metric(i, "Open rate");
    expect(opens.loading).toBeFalsy();
    expect(opens.figure).toEqual(absent("failed", "Breakdowns are not yours to read."));
  });

  it("keeps a rate it has read while a later read of one campaign is refused", () => {
    // `useReading` keeps the last answer through a failure; so does the figure.
    const stale: Reading<CampaignStats | null> = { ...answered({ sent: 1000, opens: { unique: 300, total: 400 }, clicks: { unique: 50, total: 60 } }), state: "error", error: "Connection interrupted" };
    const i = inputs({ campaigns: { rows: [A], settled: true, error: "" }, stats: new Map([["a", stale]]) });
    expect(metric(i, "Open rate").figure).toEqual(figureOf(0.3));
  });

  it("is absent with the sentence when the campaigns themselves are refused, and shaped while they are read", () => {
    const refusedCampaigns = inputs({ campaigns: { rows: [], settled: true, error: "Campaigns are not yours to read." } });
    expect(metric(refusedCampaigns, "Open rate").figure).toEqual(absent("failed", "Campaigns are not yours to read."));
    const reading = inputs({ campaigns: { rows: [], settled: false, error: "" } });
    expect(metric(reading, "Click rate")).toMatchObject({ loading: true });
  });

  it("an answer with no breakdown in it is a rate nobody can state", () => {
    const empty: Reading<CampaignStats | null> = { value: null, state: "ready", error: "", readAt: "2026-10-12T12:00:00Z", reload };
    const i = inputs({ campaigns: { rows: [A], settled: true, error: "" }, stats: new Map([["a", empty]]) });
    const opens = metric(i, "Open rate");
    expect(opens.loading).toBeFalsy();
    expect(opens.figure).toMatchObject({ kind: "absent", reason: "unmeasured" });
  });
});

describe("which breakdowns are read", () => {
  it("are those of campaigns started in the window that counted opens or clicks and sent something", () => {
    const rows = [
      sentCampaign("keep", 2, 100),
      sentCampaign("opens-only", 3, 100, { trackClicks: false }),
      sentCampaign("clicks-only", 3, 100, { trackOpens: false }),
      sentCampaign("untracked", 3, 100, { trackOpens: false, trackClicks: false }),
      sentCampaign("old", 45, 100),
      campaign({ id: "silent", status: "sending", startedAt: daysAgo(0), sentCount: 0 }),
      campaign({ id: "draft" }),
    ];
    expect(statsWanted(rows, NOW).map((c) => c.id)).toEqual(["keep", "opens-only", "clicks-only"]);
  });
});

describe("Subscribers", () => {
  it("sum active audiences and wait for every count", () => {
    const counted = new Map([["a1", figureOf(1204)], ["a2", figureOf(86)]]);
    const done = metric(inputs({ subscribers: counted }), "Subscribers");
    expect(done.figure).toEqual(figureOf(1290));
    expect(done.loading).toBeFalsy();
    expect(done.format?.(1290)).toBe("1,290");

    // One audience not counted yet: a partial sum would read as the whole.
    const partial = new Map([["a1", figureOf(1204)], ["a2", absent("unread")]]);
    const waiting = metric(inputs({ subscribers: partial }), "Subscribers");
    expect(waiting.loading).toBe(true);

    // The audiences themselves not read yet.
    expect(metric(inputs({ audiencesSettled: false }), "Subscribers").loading).toBe(true);
  });

  it("are a real zero when there is no active audience to count", () => {
    expect(metric(inputs({ subscribers: new Map() }), "Subscribers").figure).toEqual(figureOf(0));
  });

  it("are absent with the engine's sentence when one count is refused, not a sum of the rest", () => {
    const subscribers = new Map([["a1", figureOf(1204)], ["a2", absent("failed", "Counting subscribers is not yours to do.")], ["a3", absent("unread")]]);
    const refusedCount = metric(inputs({ subscribers }), "Subscribers");
    expect(refusedCount.loading).toBeFalsy();
    expect(refusedCount.figure).toEqual(absent("failed", "Counting subscribers is not yours to do."));
  });

  it("are absent when the audiences cannot be read", () => {
    const refusedAudiences = metric(inputs({ audiencesSettled: true, audiencesError: "Audiences are not yours to read." }), "Subscribers");
    expect(refusedAudiences.loading).toBeFalsy();
    expect(refusedAudiences.figure).toEqual(absent("failed", "Audiences are not yours to read."));
  });
});

describe("Active automations", () => {
  it("is the count it is given once its read has settled", () => {
    const active = metric(inputs({ activeAutomations: { count: 3, settled: true, error: "" } }), "Active automations");
    expect(active.figure).toEqual(figureOf(3));
    expect(metric(inputs(), "Active automations").figure).toEqual(figureOf(0));
  });

  it("shows its shape while read, and the sentence when refused", () => {
    expect(metric(inputs({ activeAutomations: { count: 0, settled: false, error: "" } }), "Active automations")).toMatchObject({ loading: true });
    const refusedRules = metric(inputs({ activeAutomations: { count: 0, settled: true, error: "Rules are not yours to read." } }), "Active automations");
    expect(refusedRules.loading).toBeFalsy();
    expect(refusedRules.figure).toEqual(absent("failed", "Rules are not yours to read."));
  });
});

describe("the campaign distribution", () => {
  const states = (...statuses: string[]) => statuses.map((status, n) => campaign({ id: `c${n}`, status }));

  it("divides campaigns into draft, scheduled, sending, sent and other", () => {
    const segments = campaignDistribution(
      states("draft", "draft", "scheduled", "sending", "sent", "sent", "sent", "paused", "cancelled", "failed", ""),
    );
    expect(segments).toEqual([
      { label: "Draft", count: 2 },
      { label: "Scheduled", count: 1 },
      { label: "Sending", count: 1, tone: "good" },
      { label: "Sent", count: 3 },
      { label: "Other", count: 4 },
    ]);
  });

  it("keeps the order and names with nothing to count", () => {
    expect(campaignDistribution([]).map((s) => `${s.label}:${s.count}`)).toEqual(["Draft:0", "Scheduled:0", "Sending:0", "Sent:0", "Other:0"]);
  });
});
