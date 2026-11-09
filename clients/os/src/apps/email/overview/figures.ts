import { formatCount, formatRate } from "../../../kit/format";
import { absent, figureOf, type Figure } from "../../../kit/measure";
import type { OverviewSegment } from "../../../kit/Overview";
import type { ChartPoint, StatFigure } from "../../../kit/statistics";
import { previousComparable } from "../../../kit/statistics/period";
import type { CampaignRow } from "../rows";
import type { Reading } from "../useEmail";
import { WORDS } from "../words";
import type { AudienceFigures, AutomationFigures, CampaignFigures, EmailPoint, EmailStatistics, EmailTotals } from "./statistics";

// WHAT THE OVERVIEW DRAWS (spec 9), as pure functions of what its reads have
// answered: the statistics read for the period, and the feeds and subscriber
// counts for what is true now.
//
// PURE, so the three things a figure can be stay three things and are checked
// without a DOM: a NUMBER, a REASON there is none, or a read still ON ITS WAY.
// Rendering a missing number as `0` is the failure this file exists to refuse --
// "0% opened" is a thing people act on, and a period nothing counted did not
// measure it.
//
// ===========================================================================
// WHERE EACH NUMBER COMES FROM
// ===========================================================================
//   Sent ... Suppressed  the statistics read's totals for the period, each
//                        against the period before it -- only when that period
//                        was counted from its first day (kit
//                        `previousComparable`; twelve months have none).
//   Open / Click rate    the read's, of the campaign messages sent in the
//                        period, opened or clicked on any day (a send-day
//                        cohort). Not the period's first opens over its sends:
//                        those count a message sent before the period.
//   The lists            the read's rows: each campaign, rule, journey,
//                        audience and client. A campaign's and a client's
//                        opens are by cohort too, so the rows are not parts of
//                        any total on the page and are never laid out as such.
//                        The read lists only what it measured: an empty list
//                        is "nothing" only where its figures were counted
//                        (`listsMeasured`).
//   Subscriptions        the engine's `audienceSubscriberCount` per active
//                        audience in scope, added up: a person on two
//                        audiences is two subscriptions, so it is not called
//                        subscribers. A count that reached the engine's window
//                        is a floor, and so is a sum with one in it.
//   Active automations   the rules and journeys in scope that are switched on.

/** A figure the series carries per bucket: the totals and the points name it alike. */
export type ChartKey = Exclude<keyof EmailPoint, "at">;

export interface FigureSpec {
  id: ChartKey;
  label: string;
  /** A share, written as one ("42%"); a count otherwise ("1,204"). */
  rate?: true;
}

/** The period's figures, labelled exactly as the page reads them. */
export const EMAIL_FIGURES: readonly FigureSpec[] = Object.freeze([
  { id: "sent", label: "Sent" },
  { id: "openRate", label: "Open rate", rate: true },
  { id: "clickRate", label: "Click rate", rate: true },
  { id: "unsubscribes", label: "Unsubscribes" },
  { id: "subscribes", label: "New subscribers" },
]);

/** What went wrong in delivery, a figure row of its own under its own heading. */
export const DELIVERY_FIGURES: readonly FigureSpec[] = Object.freeze([
  { id: "bounces", label: "Bounces" },
  { id: "complaints", label: "Complaints" },
  { id: "failed", label: "Failed" },
  { id: "suppressed", label: "Suppressed" },
]);

/** Why every figure is a dash when the cluster has recorded no email -- or the person reaches none. */
export const NOTHING_RECORDED = "Nothing has been recorded yet.";

/** Why a rate is a dash when only automated mail went out: a rate is of campaign mail. */
export const NO_CAMPAIGN_MAIL = "No campaign mail was sent in this period.";

export function formatOf(spec: FigureSpec): (value: number) => string {
  return spec.rate === true ? formatRate : formatCount;
}

type StatisticsReading = Pick<Reading<EmailStatistics | null>, "value" | "state" | "error">;

/**
 * Which of the read's lists were read, judged by the totals.
 *
 * THE READ LISTS ONLY WHAT IT MEASURED (component/emailstats, "Absent is never
 * zero"): a list is read only where every figure its rows carry was counted
 * through the period -- a campaign's sends AND its opens, an automation's
 * sends, an audience's subscription changes -- and otherwise arrives EMPTY,
 * which on the wire looks exactly like a period in which nothing happened. The
 * totals carry the same judgement, each null exactly where its source was not
 * walked through the period: sends (`sent`), opens and clicks (`uniqueOpens`),
 * subscription changes (`subscribes`). So an empty list says "nothing in this
 * period" only where they are measured, and "not measured" otherwise.
 */
export function listsMeasured(totals: EmailTotals): { audiences: boolean; automations: boolean; campaigns: boolean } {
  return {
    audiences: totals.subscribes !== null,
    automations: totals.sent !== null,
    campaigns: totals.sent !== null && totals.uniqueOpens !== null,
  };
}

function absentReason(stats: EmailStatistics, spec: FigureSpec): string {
  if (stats.measuredSince === null) return NOTHING_RECORDED;
  // A rate is null either because no campaign mail was sent or because its
  // sends or opens were not counted through the period. Only the first is a
  // fact about the mail, and it is said only where both were counted.
  if (spec.rate === true && listsMeasured(stats.totals).campaigns) return NO_CAMPAIGN_MAIL;
  return "";
}

function figureFrom(value: number | null, reason = ""): Figure {
  return value === null ? absent("unmeasured", reason) : figureOf(value);
}

/**
 * A figure row from the statistics read.
 *
 * WITH NO ANSWER TO DRAW there is nothing to chart, so no figure is a choice:
 * while the read is on its way each figure is its shape (and stays a choice, so
 * the row does not change kind when the answer lands); refused, each carries
 * the engine's sentence; an answer with no row in it says nothing either way.
 * A refused REFRESH is none of these: `useReading` keeps the last answer, and
 * the figures go on drawing it.
 */
export function statFigures(specs: readonly FigureSpec[], read: StatisticsReading): StatFigure[] {
  const stats = read.value;
  if (stats === null) {
    if (read.state === "idle" || read.state === "loading") {
      return specs.map((spec) => ({ id: spec.id, label: spec.label, figure: absent("unread"), format: formatOf(spec), chartable: true, loading: true }));
    }
    const nothing = read.state === "error" ? absent("failed", read.error) : absent("unmeasured");
    return specs.map((spec) => ({ id: spec.id, label: spec.label, figure: nothing, format: formatOf(spec) }));
  }
  const comparable = previousComparable(stats.prevStart, stats.measuredSince);
  return specs.map((spec) => {
    const stat: StatFigure = { id: spec.id, label: spec.label, figure: figureFrom(stats.totals[spec.id], absentReason(stats, spec)), format: formatOf(spec), chartable: true };
    // No previous figure at all unless the period before was counted throughout:
    // a change against part of a period overstates growth (kit `previousComparable`).
    if (comparable && stats.previous !== null) stat.previous = figureFrom(stats.previous[spec.id]);
    return stat;
  });
}

/** A figure bucket by bucket. A bucket nothing measured is a gap in the chart, never a zero. */
export function chartPoints(series: readonly EmailPoint[], id: ChartKey): ChartPoint[] {
  return series.map((point) => ({ at: point.at, value: point[id] }));
}

/** The previous period under the current one, only when it can be compared with; none for twelve months. */
export function previousPoints(stats: EmailStatistics, id: ChartKey): ChartPoint[] | undefined {
  if (stats.previousSeries === null || !previousComparable(stats.prevStart, stats.measuredSince)) return undefined;
  return chartPoints(stats.previousSeries, id);
}

/** A net change with its sign: "+9", "−3" (a true minus, as the kit's changes read) or "0". */
function signedCount(value: number): string {
  if (value > 0) return `+${formatCount(value)}`;
  if (value < 0) return `−${formatCount(-value)}`;
  return formatCount(0);
}

/**
 * An audience's quiet line: "12 new · 3 unsubscribed · +9 net · 1,204 subscribed".
 * The size is how many it can mail now, once counted; until then, or when it
 * cannot be counted, it is left out rather than said as nothing.
 */
export function growthLine(audience: AudienceFigures, size: Figure | undefined): string {
  const parts = [`${formatCount(audience.subscribes)} new`, `${formatCount(audience.unsubscribes)} unsubscribed`, `${signedCount(audience.net)} net`];
  if (size?.kind === "measured") parts.push(`${formatCount(size.value)}${size.atLeast ? " or more" : ""} subscribed`);
  return parts.join(" · ");
}

/** A campaign's quiet line: "42% opened · 3.1% clicked", leaving out a rate it cannot state. */
export function campaignLine(row: CampaignFigures): string | undefined {
  const parts = [row.openRate === null ? "" : `${formatRate(row.openRate)} opened`, row.clickRate === null ? "" : `${formatRate(row.clickRate)} clicked`].filter((part) => part !== "");
  return parts.length > 0 ? parts.join(" · ") : undefined;
}

/** An automation's quiet line: "120 sent · 2 failed · 1 suppressed". */
export function automationLine(row: AutomationFigures): string {
  return `${formatCount(row.sent)} sent · ${formatCount(row.failed)} failed · ${formatCount(row.suppressed)} suppressed`;
}

/** A client's quiet line: its open rate, or nothing for a client that sent no campaign mail. */
export function clientLine(row: CampaignFigures): string | undefined {
  return row.openRate === null ? undefined : `${formatRate(row.openRate)} opened`;
}

/**
 * Whether the provider reports delivery outcomes for this period: some
 * delivered or undelivered fact is in it. Where none is, "Delivered" is not
 * said at all -- a provider that never answers is not one that delivered
 * nothing.
 */
export function providerReportsOutcomes(totals: EmailTotals): boolean {
  return (totals.delivered ?? 0) > 0 || (totals.undelivered ?? 0) > 0;
}

/** A fact of what is true now: its figure, or its shape while its reads are on their way. */
export interface FactReading {
  figure: Figure;
  loading: boolean;
}

const factReading = (): FactReading => ({ figure: absent("unread"), loading: true });

/**
 * Subscriptions: the active audiences in scope, each counted by the engine,
 * added up. A count that cannot be had makes the sum something nobody can
 * state, and a partial sum would read as the whole.
 */
export function subscriptionsFact(i: { settled: boolean; error: string; counts: ReadonlyMap<string, Figure>; audienceIds: readonly string[] }): FactReading {
  if (i.error !== "") return { figure: absent("failed", i.error), loading: false };
  if (!i.settled) return factReading();
  let total = 0;
  let floor = false;
  let waiting = false;
  let failure: Figure | null = null;
  for (const id of i.audienceIds) {
    const count = i.counts.get(id) ?? absent("unread");
    if (count.kind === "measured") {
      total += count.value;
      // A count the engine stopped at its window makes the sum a floor too: "N or more".
      floor ||= count.atLeast === true;
    } else if (count.reason === "unread") waiting = true;
    else failure ??= count;
  }
  if (failure !== null) return { figure: failure, loading: false };
  if (waiting) return factReading();
  return { figure: figureOf(total, floor), loading: false };
}

/** Active automations: the rules and journeys in scope that are switched on. */
export function activeAutomationsFact(i: { count: number; settled: boolean; error: string }): FactReading {
  if (i.error !== "") return { figure: absent("failed", i.error), loading: false };
  if (!i.settled) return factReading();
  return { figure: figureOf(i.count), loading: false };
}

/**
 * A read's refusal: the engine's sentence when it left NOTHING TO SHOW, "" when
 * what it held still stands.
 *
 * A feed that holds rows and could not read them again keeps those rows (the SDK
 * holds the two together, in the state `degraded`), so its figures are still
 * true as of the last read and are drawn. Only a read with nothing to fall back
 * on is a refusal -- a dash, and "this cluster did not return" -- and what a
 * kept figure's refresh was told is said once, on the heading's line.
 */
export function refusalOf(error: string, held: number): string {
  return held > 0 ? "" : error;
}

/**
 * The campaigns in scope, divided by where they are: a current distribution,
 * not an activity history. A campaign that is paused, cancelled, failed or in a
 * state this build does not know is Other.
 */
export function campaignDistribution(rows: readonly CampaignRow[]): OverviewSegment[] {
  const tally = (status: string) => rows.filter((campaign) => campaign.status === status).length;
  const draft = tally("draft");
  const scheduled = tally("scheduled");
  const sending = tally("sending");
  const sent = tally("sent");
  return [
    { label: WORDS.campaign.draft!, count: draft },
    { label: WORDS.campaign.scheduled!, count: scheduled },
    { label: WORDS.campaign.sending!, count: sending, tone: "good" },
    { label: WORDS.campaign.sent!, count: sent },
    { label: "Other", count: rows.length - draft - scheduled - sending - sent },
  ];
}
