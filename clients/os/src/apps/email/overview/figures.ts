import { absent, figureOf, type Figure } from "../../../kit/measure";
import type { OverviewMetric, OverviewSegment } from "../../../kit/Overview";
import { campaignName, formatRate, type CampaignRow, type CampaignStats } from "../rows";
import type { Reading } from "../useEmail";
import { WORDS } from "../words";

// WHAT THE OVERVIEW MEASURES (memql#6033, Ruling 22): five figures and the shape
// of the campaigns, as pure functions of what the app's reads have answered.
//
// PURE, so the three things a figure can be stay three things and are checked
// without a DOM: a NUMBER, a REASON there is none, or a read still ON ITS WAY.
// Rendering a missing number as `0` is the failure this file exists to refuse --
// "0% opened" is a thing operators act on, and nothing here measured it.
//
// ===========================================================================
// WHERE EACH NUMBER COMES FROM
// ===========================================================================
//   Sent, 30 days       the campaigns' own `sentCount`, over those started in the
//                       window. The counters are the drain worker's exact
//                       tallies, and the campaigns feed walks every page.
//   Open / Click rate   the engine's `campaignStats` (exact counts, computed
//                       server-side -- never a client's tally of a capped page of
//                       deliveries): unique openers (clickers) over what was
//                       SENT, summed across the campaigns that counted them.
//   Subscriptions       the engine's `audienceSubscriberCount` per active audience,
//                       added up: a person on two audiences is two subscriptions,
//                       so it is not called subscribers.
//   Active automations  the rules that are On.
//
// A RATE IS OF WHAT WAS SENT, over the campaigns that asked to be counted. A
// campaign that never tracked opens has no opens to report, and adding its
// 10,000 sends to the denominator would punish every campaign that did.

/** The window "Sent, 30 days" and the rates look back over. */
export const OVERVIEW_WINDOW_DAYS = 30;
const DAY_MS = 86_400_000;

/** Why there is no rate when no campaign in the window counted it. */
export const NO_TRACKED_CAMPAIGN = `No tracked campaign in the last ${OVERVIEW_WINDOW_DAYS} days.`;

/** Why a rate cannot be stated when the engine had no breakdown for a campaign sent in the window: said of that campaign. */
const noBreakdown = (campaign: string): string => `No breakdown was returned for “${campaign}”.`;

/**
 * A read's refusal: the engine's sentence when it left NOTHING TO SHOW, "" when
 * what it held still stands.
 *
 * A feed that holds rows and could not read them again keeps those rows (the SDK
 * holds the two together, in the state `degraded`), so its figures are still
 * true as of the last read and are drawn. Only a read with nothing to fall back
 * on is a refusal -- a dash, and "this cluster did not return" -- and what a
 * kept figure's refresh was told is said once, by the page, beside the figures it
 * kept. The page hands every `error` below through this.
 */
export function refusalOf(error: string, held: number): string {
  return held > 0 ? "" : error;
}

/**
 * What the Overview is made from. Each read says whether it has answered and,
 * when it was refused with nothing to show, why (`refusalOf`).
 */
export interface OverviewInputs {
  campaigns: { rows: CampaignRow[]; settled: boolean; error: string };
  /** campaignId -> its breakdown, for the campaigns `statsWanted` names. */
  stats: ReadonlyMap<string, Reading<CampaignStats | null>>;
  /** Active audiences only: audienceId -> how many people it can mail now. */
  subscribers: ReadonlyMap<string, Figure>;
  audiencesSettled: boolean;
  /** The audiences list was refused: the subscribers cannot be added up, and nothing says it is zero. */
  audiencesError?: string;
  /** Rules that are On now; rules and journeys once journeys exist. */
  activeAutomations: { count: number; settled: boolean; error: string };
  now: Date;
}

const count = (value: number): string => value.toLocaleString("en-US");

/** A figure whose read is still on its way: the shape of a figure, never a caption. */
const reading = (label: string): OverviewMetric => ({ label, figure: absent("unread"), loading: true });

/** A figure whose read was refused: an em dash, and the engine's sentence on it. */
const refused = (label: string, sentence: string): OverviewMetric => ({ label, figure: absent("failed", sentence) });

/** A figure that has no number and says why, under the dash. */
const unmeasured = (label: string, reason: string): OverviewMetric => ({
  label,
  figure: absent("unmeasured", reason),
  detail: reason,
});

/** Whether a campaign went out inside the window. A start nobody can read, or no start at all (a draft), did not. */
export function startedInWindow(campaign: CampaignRow, now: Date): boolean {
  const started = Date.parse(campaign.startedAt);
  if (!Number.isFinite(started)) return false;
  // No upper bound: a clock a little ahead of this one may have written the
  // start, and a send under way should not leave the figure for that.
  return started >= now.getTime() - OVERVIEW_WINDOW_DAYS * DAY_MS;
}

/**
 * The campaigns whose breakdown the Overview reads: started in the window,
 * counting opens or clicks, with something sent. Anything else cannot add to a
 * rate, so reading it would be a request for nothing -- and the figures and the
 * reads ask this one question, so they cannot disagree about who is in them.
 */
export function statsWanted(campaigns: readonly CampaignRow[], now: Date): CampaignRow[] {
  return campaigns.filter(
    (campaign) => campaign.id !== "" && startedInWindow(campaign, now) && (campaign.trackOpens || campaign.trackClicks) && campaign.sentCount > 0,
  );
}

function sentMetric(i: OverviewInputs): OverviewMetric {
  const label = "Sent, 30 days";
  if (i.campaigns.error !== "") return refused(label, i.campaigns.error);
  if (!i.campaigns.settled) return reading(label);
  const sent = i.campaigns.rows.filter((campaign) => startedInWindow(campaign, i.now)).reduce((sum, campaign) => sum + campaign.sentCount, 0);
  return { label, figure: figureOf(sent), format: count };
}

/** Unique openers (clickers) over what was sent, across the campaigns that counted them. */
function rateMetric(label: string, kind: "opens" | "clicks", i: OverviewInputs): OverviewMetric {
  if (i.campaigns.error !== "") return refused(label, i.campaigns.error);
  if (!i.campaigns.settled) return reading(label);

  const counted = statsWanted(i.campaigns.rows, i.now).filter((campaign) => (kind === "opens" ? campaign.trackOpens : campaign.trackClicks));
  if (counted.length === 0) return unmeasured(label, NO_TRACKED_CAMPAIGN);

  let unique = 0;
  let sent = 0;
  let waiting = false;
  let refusal = "";
  let reason = "";
  for (const campaign of counted) {
    const breakdown = i.stats.get(campaign.id);
    const stats = breakdown?.value ?? null;
    if (stats === null) {
      if (breakdown === undefined || breakdown.state === "loading" || breakdown.state === "idle") waiting = true;
      else if (breakdown.state === "error") refusal ||= breakdown.error;
      else reason ||= noBreakdown(campaignName(campaign));
      continue;
    }
    const figure = kind === "opens" ? stats.opensUnique : stats.clicksUnique;
    if (figure.value === null) {
      reason ||= figure.absentBecause;
      continue;
    }
    unique += figure.value;
    sent += campaign.sentCount;
  }

  // WHAT IS KNOWN TO BE ABSENT OUTRANKS WHAT IS STILL ARRIVING: a rate that one
  // campaign makes impossible to state does not wait for the others, and a
  // skeleton must never conceal a refusal.
  if (refusal !== "") return refused(label, refusal);
  if (reason !== "") return unmeasured(label, reason);
  if (waiting) return reading(label);
  // `counted` only holds campaigns that sent something, so `sent` is not zero.
  return { label, figure: figureOf(unique / sent), format: formatRate };
}

function subscriptionsMetric(i: OverviewInputs): OverviewMetric {
  const label = "Subscriptions";
  if (i.audiencesError !== undefined && i.audiencesError !== "") return refused(label, i.audiencesError);
  if (!i.audiencesSettled) return reading(label);
  let total = 0;
  let waiting = false;
  let failure: Figure | null = null;
  for (const figure of i.subscribers.values()) {
    if (figure.kind === "measured") total += figure.value;
    else if (figure.reason === "unread") waiting = true;
    else failure ??= figure;
  }
  // A count that cannot be had makes the sum something nobody can state; the
  // sum of the others would read as the whole.
  if (failure !== null) return { label, figure: failure };
  if (waiting) return reading(label);
  return { label, figure: figureOf(total), format: count };
}

function automationsMetric(i: OverviewInputs): OverviewMetric {
  const label = "Active automations";
  if (i.activeAutomations.error !== "") return refused(label, i.activeAutomations.error);
  if (!i.activeAutomations.settled) return reading(label);
  return { label, figure: figureOf(i.activeAutomations.count), format: count };
}

/**
 * A reason is said once. Two figures that give the same one (neither rate has a
 * tracked campaign to be made of) keep their dashes, and the reason stands under
 * the first; it stays on the second's hover, in the figure itself.
 */
function saidOnce(metrics: OverviewMetric[]): OverviewMetric[] {
  const said = new Set<string>();
  return metrics.map(({ detail, ...metric }) => {
    if (detail === undefined || said.has(detail)) return metric;
    said.add(detail);
    return { ...metric, detail };
  });
}

/** The five figures, labelled exactly as the page reads them. */
export function overviewMetrics(i: OverviewInputs): OverviewMetric[] {
  return saidOnce([
    sentMetric(i),
    rateMetric("Open rate", "opens", i),
    rateMetric("Click rate", "clicks", i),
    subscriptionsMetric(i),
    automationsMetric(i),
  ]);
}

/**
 * Every campaign the cluster has, divided by where it is: a current
 * distribution, not an activity history (nothing here is a time series). A
 * campaign that is paused, cancelled, failed or in a state this build does not
 * know is Other.
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
