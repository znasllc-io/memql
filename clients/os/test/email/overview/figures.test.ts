import { describe, expect, it } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  DELIVERY_FIGURES,
  EMAIL_FIGURES,
  NOTHING_RECORDED,
  NO_CAMPAIGN_MAIL,
  activeAutomationsFact,
  automationLine,
  campaignDistribution,
  campaignLine,
  chartPoints,
  clientLine,
  growthLine,
  listsMeasured,
  previousPoints,
  providerReportsOutcomes,
  refusalOf,
  statFigures,
  subscriptionsFact,
} from "../../../src/apps/email/overview/figures";
import { parseEmailStatistics, type EmailStatistics } from "../../../src/apps/email/overview/statistics";
import { campaignFromRow } from "../../../src/apps/email/rows";
import type { Reading } from "../../../src/apps/email/useEmail";
import { absent, figureOf } from "../../../src/kit/measure";
import { campaignRow, statisticsRow, twelveMonthsStatisticsRow, unmeasuredStatisticsRow, type StatisticsOver } from "../harness";

// WHAT THE OVERVIEW DRAWS FROM THE STATISTICS READ, AS PURE FUNCTIONS (spec 9).
//
// A figure is one of three things -- a number, a reason there is none, or a
// read still on its way -- and drawing one as another is the failure this file
// refuses: "0% opened" is a thing people act on, and a period nothing counted
// did not measure it. A change is drawn only against a previous period that
// was measured throughout (kit `previousComparable`).

const reload = async () => {};

/** The statistics read, answered. */
function answered(over: StatisticsOver = {}): Reading<EmailStatistics | null> {
  return { value: parseEmailStatistics(statisticsRow(over)), state: "ready", error: "", readAt: "2026-11-08T12:00:00Z", reload };
}

function answeredWith(row: Row): Reading<EmailStatistics | null> {
  return { value: parseEmailStatistics(row), state: "ready", error: "", readAt: "2026-11-08T12:00:00Z", reload };
}

const reading: Reading<EmailStatistics | null> = { value: null, state: "loading", error: "", readAt: "", reload };

function figure(read: Reading<EmailStatistics | null>, id: string) {
  const found = [...statFigures(EMAIL_FIGURES, read), ...statFigures(DELIVERY_FIGURES, read)].find((candidate) => candidate.id === id);
  if (found === undefined) throw new Error(`no figure ${id}`);
  return found;
}

describe("the figures", () => {
  it("are named exactly, in this order, for the period and for its delivery problems", () => {
    expect(EMAIL_FIGURES.map((spec) => spec.label)).toEqual(["Sent", "Open rate", "Click rate", "Unsubscribes", "New subscribers"]);
    expect(DELIVERY_FIGURES.map((spec) => spec.label)).toEqual(["Bounces", "Complaints", "Failed", "Suppressed"]);
    expect(statFigures(EMAIL_FIGURES, answered()).map((stat) => stat.id)).toEqual(["sent", "openRate", "clickRate", "unsubscribes", "subscribes"]);
  });

  it("are the period's totals, every one of them chartable, the rates written as rates", () => {
    const read = answered();
    expect(figure(read, "sent")).toMatchObject({ label: "Sent", figure: figureOf(1204), previous: figureOf(1075), chartable: true });
    expect(figure(read, "sent").format?.(1204)).toBe("1,204");
    // The open rate is the read's cohort rate, not the period's first opens over its sends.
    expect(figure(read, "openRate").figure).toEqual(figureOf(0.42));
    expect(figure(read, "openRate").format?.(0.42)).toBe("42%");
    expect(figure(read, "clickRate").format?.(0.031)).toBe("3.1%");
    expect(figure(read, "subscribes")).toMatchObject({ figure: figureOf(212), previous: figureOf(0) });
    expect(figure(read, "bounces")).toMatchObject({ label: "Bounces", figure: figureOf(7), chartable: true });
    expect(statFigures(DELIVERY_FIGURES, read).every((stat) => stat.chartable === true)).toBe(true);
  });

  it("are their shapes while the read is on its way, never a number or a dash", () => {
    for (const stat of statFigures(EMAIL_FIGURES, reading)) {
      expect(stat.loading).toBe(true);
      expect(stat.figure.kind).toBe("absent");
    }
    // Not asked yet (no connection) is still not an answer.
    expect(statFigures(EMAIL_FIGURES, { ...reading, state: "idle" }).every((stat) => stat.loading === true)).toBe(true);
  });

  it("carry the engine's sentence when the read was refused with nothing to show, and choose no chart", () => {
    const refused = statFigures(EMAIL_FIGURES, { value: null, state: "error", error: "Email statistics are not yours to read." });
    for (const stat of refused) {
      expect(stat.loading).toBeFalsy();
      expect(stat.figure).toEqual(absent("failed", "Email statistics are not yours to read."));
      expect(stat.chartable).toBeFalsy();
      expect(stat.previous).toBeUndefined();
    }
  });

  it("keep the last answer through a refused refresh", () => {
    const kept: Reading<EmailStatistics | null> = { ...answered(), state: "error", error: "The cluster did not answer in time." };
    expect(figure(kept, "sent").figure).toEqual(figureOf(1204));
  });

  it("are a dash that says why when nothing was recorded, never zero", () => {
    const read = answeredWith(unmeasuredStatisticsRow());
    for (const stat of [...statFigures(EMAIL_FIGURES, read), ...statFigures(DELIVERY_FIGURES, read)]) {
      expect(stat.figure).toEqual(absent("unmeasured", NOTHING_RECORDED));
      // Nothing measured before is no change, not a fall to nothing.
      expect(stat.previous === undefined || stat.previous.kind === "absent").toBe(true);
    }
    expect(NOTHING_RECORDED).toBe("Nothing has been recorded yet.");
    // An answer with no row in it has nothing to say for itself either way.
    const empty = statFigures(EMAIL_FIGURES, { value: null, state: "ready", error: "" });
    expect(empty.every((stat) => stat.figure.kind === "absent" && !stat.loading && !stat.chartable)).toBe(true);
  });

  it("leave a rate unstated when no campaign mail was sent, and say so", () => {
    // Rule and journey mail went out; no campaign did, so there is nothing for a rate to be a share of.
    const read = answered({ totals: { sent: 160, openRate: null, clickRate: null } });
    expect(figure(read, "sent").figure).toEqual(figureOf(160));
    expect(figure(read, "openRate").figure).toEqual(absent("unmeasured", NO_CAMPAIGN_MAIL));
    expect(figure(read, "clickRate").figure).toEqual(absent("unmeasured", NO_CAMPAIGN_MAIL));
    expect(NO_CAMPAIGN_MAIL).toBe("No campaign mail was sent in this period.");
  });

  it("never say no campaign mail was sent when the opens were not counted through the period", () => {
    // The rollup has walked the sends and not yet the opens: the rate is unmeasured, not a rate of nothing.
    const read = answered({ totals: { uniqueOpens: null, uniqueClicks: null, opens: null, clicks: null, openRate: null, clickRate: null } });
    expect(figure(read, "sent").figure).toEqual(figureOf(1204));
    expect(figure(read, "openRate").figure).toEqual(absent("unmeasured"));
    expect(figure(read, "clickRate").figure).toEqual(absent("unmeasured"));
  });

  it("compare only with a previous period that was counted from its first day", () => {
    // Counting began on Sep 20; the thirty days before began on Sep 10, ten of them uncounted.
    const partial = answered({ measuredSince: "2026-09-20" });
    expect(figure(partial, "sent").figure).toEqual(figureOf(1204));
    expect(figure(partial, "sent").previous).toBeUndefined();
    expect(previousPoints(partial.value!, "sent")).toBeUndefined();

    // Counting began on the previous period's first day: it is compared with.
    const whole = answered({ measuredSince: "2026-09-10" });
    expect(figure(whole, "sent").previous).toEqual(figureOf(1075));
    expect(previousPoints(whole.value!, "sent")).toHaveLength(30);

    // Twelve months have no previous period at all.
    const year = answeredWith(twelveMonthsStatisticsRow());
    expect(statFigures(EMAIL_FIGURES, year).every((stat) => stat.previous === undefined)).toBe(true);
    expect(previousPoints(year.value!, "sent")).toBeUndefined();
  });

  it("chart a figure bucket by bucket, an unmeasured bucket as a gap", () => {
    const stats = parseEmailStatistics(statisticsRow());
    const sent = chartPoints(stats.series, "sent");
    expect(sent).toHaveLength(30);
    expect(sent[5]).toEqual({ at: "2026-10-15T00:00:00Z", value: 64 });
    expect(chartPoints(stats.series, "openRate")[20]).toEqual({ at: "2026-10-30T00:00:00Z", value: 0.55 });
    const unmeasured = parseEmailStatistics(unmeasuredStatisticsRow());
    expect(chartPoints(unmeasured.series, "sent").every((point) => point.value === null)).toBe(true);
  });
});

describe("a row's quiet line", () => {
  it("says an audience's growth, and its size once it has been counted", () => {
    const audience = { id: "a-news", subscribes: 12, unsubscribes: 3, net: 9 };
    expect(growthLine(audience, figureOf(1204))).toBe("12 new · 3 unsubscribed · +9 net · 1,204 subscribed");
    // Not counted yet, or not countable: the size is left out, never a zero.
    expect(growthLine(audience, absent("unread"))).toBe("12 new · 3 unsubscribed · +9 net");
    expect(growthLine(audience, absent("failed", "Counting subscribers is not yours to do."))).toBe("12 new · 3 unsubscribed · +9 net");
    expect(growthLine(audience, undefined)).toBe("12 new · 3 unsubscribed · +9 net");
    // A shrinking audience reads with a true minus; a count that stopped at the engine's window is a floor.
    expect(growthLine({ id: "a", subscribes: 2, unsubscribes: 5, net: -3 }, figureOf(5000, true))).toBe("2 new · 5 unsubscribed · −3 net · 5,000 or more subscribed");
    expect(growthLine({ id: "a", subscribes: 4, unsubscribes: 4, net: 0 }, figureOf(1_340))).toBe("4 new · 4 unsubscribed · 0 net · 1,340 subscribed");
  });

  it("says a campaign's rates, leaving out the one it cannot state", () => {
    const row = { id: "c", sent: 600, uniqueOpens: 300, uniqueClicks: 34, openRate: 0.42, clickRate: 0.031 };
    expect(campaignLine(row)).toBe("42% opened · 3.1% clicked");
    expect(campaignLine({ ...row, clickRate: null })).toBe("42% opened");
    expect(campaignLine({ ...row, openRate: null })).toBe("3.1% clicked");
    expect(campaignLine({ ...row, openRate: null, clickRate: null })).toBeUndefined();
  });

  it("says an automation's sent, failed and suppressed", () => {
    expect(automationLine({ id: "r", kind: "rule", sent: 1204, failed: 2, suppressed: 1 })).toBe("1,204 sent · 2 failed · 1 suppressed");
    expect(automationLine({ id: "j", kind: "journey", sent: 40, failed: 0, suppressed: 0 })).toBe("40 sent · 0 failed · 0 suppressed");
  });

  it("says a client's open rate, and nothing for a client that sent no campaign mail", () => {
    const row = { id: "acct-a", sent: 900, uniqueOpens: 400, uniqueClicks: 30, openRate: 0.45, clickRate: 0.033 };
    expect(clientLine(row)).toBe("45% opened");
    expect(clientLine({ ...row, openRate: null, clickRate: null })).toBeUndefined();
  });
});

// A LIST IS READ ONLY WHERE EVERY FIGURE ITS ROWS CARRY IS MEASURED
// (component/emailstats, "Absent is never zero"), and otherwise arrives empty --
// so an empty list is "nothing in the period" only where its figures were counted.
describe("whether a list was measured", () => {
  const totals = (over: Record<string, number | null>) => parseEmailStatistics(statisticsRow({ totals: over })).totals;

  it("is whether the figures its rows carry were counted through the period", () => {
    expect(listsMeasured(totals({}))).toEqual({ audiences: true, automations: true, campaigns: true });
    // Subscription changes not walked: audiences cannot be listed.
    expect(listsMeasured(totals({ subscribes: null, unsubscribes: null }))).toEqual({ audiences: false, automations: true, campaigns: true });
    // Opens not walked: a campaign's row carries its opens, so campaigns cannot be listed; automations can.
    expect(listsMeasured(totals({ uniqueOpens: null, uniqueClicks: null }))).toEqual({ audiences: true, automations: true, campaigns: false });
    // Sends not walked: neither.
    expect(listsMeasured(totals({ sent: null }))).toEqual({ audiences: true, automations: false, campaigns: false });
    expect(listsMeasured(parseEmailStatistics(unmeasuredStatisticsRow()).totals)).toEqual({ audiences: false, automations: false, campaigns: false });
  });
});

describe("whether the provider reports outcomes", () => {
  it("is whether the period holds a delivered or an undelivered fact", () => {
    const totals = (delivered: number | null, undelivered: number | null) => parseEmailStatistics(statisticsRow({ totals: { delivered, undelivered } })).totals;
    expect(providerReportsOutcomes(totals(1180, 9))).toBe(true);
    // None delivered and some refused is still an outcome reported.
    expect(providerReportsOutcomes(totals(0, 4))).toBe(true);
    expect(providerReportsOutcomes(totals(0, 0))).toBe(false);
    expect(providerReportsOutcomes(totals(null, null))).toBe(false);
  });
});

// A SUBSCRIPTION, NOT A SUBSCRIBER: the fact adds up what each active audience
// in scope can mail now, so a person on two audiences is counted twice.
describe("Subscriptions", () => {
  const settled = { settled: true, error: "" };

  it("sums the audiences it is given, and waits for every count", () => {
    const counts = new Map([["a1", figureOf(1204)], ["a2", figureOf(86)], ["other", figureOf(500)]]);
    expect(subscriptionsFact({ ...settled, counts, audienceIds: ["a1", "a2"] })).toEqual({ figure: figureOf(1290), loading: false });

    // One audience not counted yet: a partial sum would read as the whole.
    const partial = new Map([["a1", figureOf(1204)], ["a2", absent("unread")]]);
    expect(subscriptionsFact({ ...settled, counts: partial, audienceIds: ["a1", "a2"] }).loading).toBe(true);
    // A count nobody has asked for yet is still on its way.
    expect(subscriptionsFact({ ...settled, counts: new Map(), audienceIds: ["a1"] }).loading).toBe(true);
    // The audiences themselves not read yet.
    expect(subscriptionsFact({ settled: false, error: "", counts, audienceIds: [] }).loading).toBe(true);
  });

  it("is at least the sum when a count stopped at the engine's window", () => {
    const counts = new Map([["a1", figureOf(5000, true)], ["a2", figureOf(1204)]]);
    expect(subscriptionsFact({ ...settled, counts, audienceIds: ["a1", "a2"] }).figure).toEqual(figureOf(6204, true));
  });

  it("is a real zero when there is no active audience to count", () => {
    expect(subscriptionsFact({ ...settled, counts: new Map(), audienceIds: [] })).toEqual({ figure: figureOf(0), loading: false });
  });

  it("is absent with the engine's sentence when a count or the audiences are refused, never the sum of the rest", () => {
    const counts = new Map([["a1", figureOf(1204)], ["a2", absent("failed", "Counting subscribers is not yours to do.")], ["a3", absent("unread")]]);
    expect(subscriptionsFact({ ...settled, counts, audienceIds: ["a1", "a2", "a3"] })).toEqual({ figure: absent("failed", "Counting subscribers is not yours to do."), loading: false });
    expect(subscriptionsFact({ settled: true, error: "Audiences are not yours to read.", counts, audienceIds: [] })).toEqual({ figure: absent("failed", "Audiences are not yours to read."), loading: false });
  });
});

describe("Active automations", () => {
  it("is the count once its reads have settled, its shape before, and the sentence when refused", () => {
    expect(activeAutomationsFact({ count: 3, settled: true, error: "" })).toEqual({ figure: figureOf(3), loading: false });
    expect(activeAutomationsFact({ count: 0, settled: true, error: "" }).figure).toEqual(figureOf(0));
    expect(activeAutomationsFact({ count: 0, settled: false, error: "" }).loading).toBe(true);
    expect(activeAutomationsFact({ count: 0, settled: true, error: "Rules are not yours to read." })).toEqual({ figure: absent("failed", "Rules are not yours to read."), loading: false });
  });
});

describe("a read's refusal", () => {
  it("is the engine's sentence only when the read left nothing to show", () => {
    expect(refusalOf("Campaigns are not yours to read.", 0)).toBe("Campaigns are not yours to read.");
    // A feed that holds rows and could not refresh them keeps showing them: it is not a refusal.
    expect(refusalOf("Campaigns are not yours to read.", 3)).toBe("");
    expect(refusalOf("", 0)).toBe("");
  });
});

describe("the campaign distribution", () => {
  const states = (...statuses: string[]) => statuses.map((status, n) => campaignFromRow(campaignRow({ id: `c${n}`, status })));

  it("divides campaigns into draft, scheduled, sending, sent and other", () => {
    expect(campaignDistribution(states("draft", "draft", "scheduled", "sending", "sent", "sent", "sent", "paused", "cancelled", "failed", ""))).toEqual([
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
