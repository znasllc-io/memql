import { useMemo } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { EMAIL_PERIODS, isPeriod, type Bucket, type Period } from "../../../kit/statistics/period";
import { useOsConnection } from "../../../live/connection";
import { useReading, type Reading } from "../useEmail";

// THE EMAIL STATISTICS READ (spec 7 and 9): `builtin emailStatistics`, a period
// of email figures for the organizations the caller reaches, answered as ONE
// node whose payload is `EmailStatistics.Payload` in
// component/emailstats/statistics.go. The keys below are that payload's, and
// the types follow it where the brief's draft and the payload disagree: the
// payload also says `hasPrevious`, `prevStart` and `prevEnd`, and the page
// needs `prevStart` to decide whether a change can be drawn at all.
//
// ===========================================================================
// ABSENT IS NOT ZERO, AND THE PARSE IS WHERE THAT IS EASIEST TO BREAK
// ===========================================================================
// The server writes every figure nothing measured as a PRESENT JSON NULL: a
// day before the cluster's first recorded fact, every figure of a caller who
// reaches nothing, a rate with no campaign mail to be a share of. Each of those
// reads here as `null`, and nothing in this file turns a null into a 0 -- a
// helper that does (`rowNumber`) would draw "0% opened" over a period nobody
// counted, which is the one mistake these pages exist to refuse.
//
// ===========================================================================
// TWO WAYS TO COUNT AN OPEN (spec D18)
// ===========================================================================
// `totals.openRate` and `clickRate` are of the CAMPAIGN messages sent in the
// period, opened (clicked) on any day: a send-day cohort. `totals.uniqueOpens`
// counts the messages first opened IN the period, whenever they were sent. A
// campaign's row and a client's row count by cohort too. So the rows are not
// parts of the totals, and the page never lays them out as if they were.
//
// ONE READ, NO CLOCK. The surface that shows it mounts the kit's `AutoRefresh`
// on `reload` every `STATISTICS_REFRESH_MS` (spec 7: fifteen minutes for a
// daily period), which reads only while the pane is seen; the engine's result
// cache answers a repeat within its minute.

/** How often the Overview reads its statistics again while it is seen (spec 7). */
export const STATISTICS_REFRESH_MS = 900_000;

/** A period's figures. Null is "not measured", never zero. */
export interface EmailTotals {
  sent: number | null;
  delivered: number | null;
  undelivered: number | null;
  failed: number | null;
  skipped: number | null;
  suppressed: number | null;
  opens: number | null;
  clicks: number | null;
  /** The messages first opened in the period, whenever they were sent. */
  uniqueOpens: number | null;
  uniqueClicks: number | null;
  unsubscribes: number | null;
  bounces: number | null;
  complaints: number | null;
  subscribes: number | null;
  /** Of the campaign messages sent in the period, the share opened on any day; null when none was sent. */
  openRate: number | null;
  clickRate: number | null;
}

/** One bucket of a series, starting `at`. Each figure counted as the totals count it. */
export interface EmailPoint {
  at: string;
  sent: number | null;
  uniqueOpens: number | null;
  uniqueClicks: number | null;
  unsubscribes: number | null;
  subscribes: number | null;
  bounces: number | null;
  complaints: number | null;
  failed: number | null;
  suppressed: number | null;
  openRate: number | null;
  clickRate: number | null;
}

/** One campaign's sends in the period, or one client's (`accounts`); its opens and clicks by send cohort. */
export interface CampaignFigures {
  id: string;
  sent: number;
  uniqueOpens: number;
  uniqueClicks: number;
  openRate: number | null;
  clickRate: number | null;
}

/** One rule's or journey's mail in the period. Automated mail has no opens or clicks: it is not tracked. */
export interface AutomationFigures {
  id: string;
  kind: "rule" | "journey";
  sent: number;
  failed: number;
  suppressed: number;
}

/** One audience's subscription changes in the period; `net` is subscribes less unsubscribes. */
export interface AudienceFigures {
  id: string;
  subscribes: number;
  unsubscribes: number;
  net: number;
}

export interface EmailStatistics {
  period: Period;
  bucket: Bucket;
  start: string;
  end: string;
  /** Whether the read had a previous period to answer for. Twelve months have none (spec D4). */
  hasPrevious: boolean;
  /** The previous period's bounds; null without one. */
  prevStart: string | null;
  prevEnd: string | null;
  /** The cluster's first day with a recorded fact (YYYY-MM-DD, UTC); null when nothing was ever recorded, or the caller reaches nothing. */
  measuredSince: string | null;
  totals: EmailTotals;
  /** Null for twelve months. */
  previous: EmailTotals | null;
  /** One point per bucket, oldest first. */
  series: EmailPoint[];
  /** The previous period's points at the same offsets; null for twelve months. */
  previousSeries: EmailPoint[] | null;
  /** The leading campaigns with sends in the period, in the read's order: most unique opens, then unique clicks. */
  campaigns: CampaignFigures[];
  /** Each rule and journey with mail in the period, most sent first. */
  automations: AutomationFigures[];
  /** Each audience with subscription changes in the period, the largest net change first. */
  audiences: AudienceFigures[];
  /** Each client with sends, most sent first; listed only when the read covers more than one. */
  accounts: CampaignFigures[];
}

type Fields = Record<string, unknown>;

function fieldsOf(value: unknown): Fields | null {
  return typeof value === "object" && value !== null && !Array.isArray(value) ? (value as Fields) : null;
}

/**
 * A figure: a finite number, or null for anything else -- a JSON null above
 * all. A numeric string is read as its number, because a wire that carries a
 * large integer as a string still carries a count somebody made.
 */
function figure(value: unknown): number | null {
  if (typeof value === "number") return Number.isFinite(value) ? value : null;
  if (typeof value === "string" && value.trim() !== "") {
    const parsed = Number(value);
    return Number.isFinite(parsed) ? parsed : null;
  }
  return null;
}

function text(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function textOrNull(value: unknown): string | null {
  return typeof value === "string" && value !== "" ? value : null;
}

function totalsOf(value: unknown): EmailTotals {
  const f = fieldsOf(value) ?? {};
  return {
    sent: figure(f.sent),
    delivered: figure(f.delivered),
    undelivered: figure(f.undelivered),
    failed: figure(f.failed),
    skipped: figure(f.skipped),
    suppressed: figure(f.suppressed),
    opens: figure(f.opens),
    clicks: figure(f.clicks),
    uniqueOpens: figure(f.uniqueOpens),
    uniqueClicks: figure(f.uniqueClicks),
    unsubscribes: figure(f.unsubscribes),
    bounces: figure(f.bounces),
    complaints: figure(f.complaints),
    subscribes: figure(f.subscribes),
    openRate: figure(f.openRate),
    clickRate: figure(f.clickRate),
  };
}

function pointOf(value: unknown): EmailPoint {
  const f = fieldsOf(value) ?? {};
  return {
    at: text(f.at),
    sent: figure(f.sent),
    uniqueOpens: figure(f.uniqueOpens),
    uniqueClicks: figure(f.uniqueClicks),
    unsubscribes: figure(f.unsubscribes),
    subscribes: figure(f.subscribes),
    bounces: figure(f.bounces),
    complaints: figure(f.complaints),
    failed: figure(f.failed),
    suppressed: figure(f.suppressed),
    openRate: figure(f.openRate),
    clickRate: figure(f.clickRate),
  };
}

// A LIST ENTRY THE PAGE CAN NEITHER NAME NOR RANK IS LEFT OUT: one with no id,
// no count to place it by, or a kind of automation this build does not know.
// The server always writes them whole; a row drawn from half of one would be a
// figure nobody stated.

function sendFiguresOf(value: unknown): CampaignFigures | null {
  const f = fieldsOf(value);
  if (f === null) return null;
  const id = text(f.id);
  const sent = figure(f.sent);
  const uniqueOpens = figure(f.uniqueOpens);
  const uniqueClicks = figure(f.uniqueClicks);
  if (id === "" || sent === null || uniqueOpens === null || uniqueClicks === null) return null;
  return { id, sent, uniqueOpens, uniqueClicks, openRate: figure(f.openRate), clickRate: figure(f.clickRate) };
}

function automationFiguresOf(value: unknown): AutomationFigures | null {
  const f = fieldsOf(value);
  if (f === null) return null;
  const id = text(f.id);
  const kind = f.kind === "rule" || f.kind === "journey" ? f.kind : null;
  const sent = figure(f.sent);
  const failed = figure(f.failed);
  const suppressed = figure(f.suppressed);
  if (id === "" || kind === null || sent === null || failed === null || suppressed === null) return null;
  return { id, kind, sent, failed, suppressed };
}

function audienceFiguresOf(value: unknown): AudienceFigures | null {
  const f = fieldsOf(value);
  if (f === null) return null;
  const id = text(f.id);
  const subscribes = figure(f.subscribes);
  const unsubscribes = figure(f.unsubscribes);
  if (id === "" || subscribes === null || unsubscribes === null) return null;
  // `net` is the server's subscribes less unsubscribes; worked out the same way when it is missing.
  return { id, subscribes, unsubscribes, net: figure(f.net) ?? subscribes - unsubscribes };
}

function listOf<T>(value: unknown, entry: (value: unknown) => T | null): T[] {
  if (!Array.isArray(value)) return [];
  return value.map(entry).filter((parsed): parsed is T => parsed !== null);
}

/** The statistics node's payload, as the page holds it. */
export function parseEmailStatistics(row: Row): EmailStatistics {
  // The page draws the period it chose, never this echo of it; a period Email
  // does not offer can only be a defect, and reads as the default rather than
  // as a period nobody asked for.
  const period: Period = isPeriod(row.period, EMAIL_PERIODS) ? row.period : "30d";
  const bucket: Bucket = row.bucket === "hour" || row.bucket === "day" || row.bucket === "month" ? row.bucket : period === "12m" ? "month" : "day";
  return {
    period,
    bucket,
    start: text(row.start),
    end: text(row.end),
    hasPrevious: row.hasPrevious === true,
    prevStart: textOrNull(row.prevStart),
    prevEnd: textOrNull(row.prevEnd),
    measuredSince: textOrNull(row.measuredSince),
    totals: totalsOf(row.totals),
    previous: fieldsOf(row.previous) === null ? null : totalsOf(row.previous),
    series: Array.isArray(row.series) ? row.series.map(pointOf) : [],
    previousSeries: Array.isArray(row.previousSeries) ? row.previousSeries.map(pointOf) : null,
    campaigns: listOf(row.campaigns, sendFiguresOf),
    automations: listOf(row.automations, automationFiguresOf),
    audiences: listOf(row.audiences, audienceFiguresOf),
    accounts: listOf(row.accounts, sendFiguresOf),
  };
}

/**
 * The statistics of one period, for the clients `accountIds` names -- none for
 * every client in scope, which is what the client scope sends for All. The ids
 * are sent as they are: the server narrows them to what the caller reaches
 * (spec D11), so a stored client the person no longer reaches answers nothing,
 * never another client's figures.
 *
 * A NEW PERIOD OR CLIENT IS A NEW QUESTION. `useReading` starts over for it, so
 * the page draws shapes rather than the last period's figures under the new
 * period's name; a refresh of the same question keeps its last answer, through
 * a refusal too.
 */
export function useEmailStatistics({ accountIds, period }: { accountIds: readonly string[]; period: Period }): Reading<EmailStatistics | null> {
  const connection = useOsConnection();
  const query = connection?.query ?? null;
  // The ids as a value: callers hand a new array on every render.
  const ids = JSON.stringify(accountIds);
  const read = useMemo(() => {
    if (query === null) return null;
    const named = JSON.parse(ids) as string[];
    return async (signal: AbortSignal): Promise<EmailStatistics | null> => {
      const first = (await query.emailStatistics({ accountIds: named, period }, { signal })).rows()[0];
      return first === undefined ? null : parseEmailStatistics(first);
    };
  }, [query, ids, period]);
  return useReading<EmailStatistics | null>(null, read, [query, ids, period]);
}
