import type { FlowStep, Trigger } from "./automations/flowModel";
import { useMemo } from "react";
import { rowObject, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { boolOr, flatten, stringsOf } from "../../kit/rows";

// The wire rows the Email app renders, projected into the shapes its
// surfaces read.
//
// PURE, and separate from every component, for the reason apps/accounts/rows.ts
// is: a projection asserted through render() is asserted through three layers
// that can each fail for unrelated reasons. Everything here is a function of a
// row (or of a plain object) and is unit-testable with no browser, no cluster
// and no React.
//
// ===========================================================================
// EVERY FIELD BELOW IS ONE A SHAPE PROJECTS, NOT ONE A CONCEPT DECLARES
// ===========================================================================
// A concept field is not a readable field, and the omission is silent (the
// Training app's rule). These projections were written against
// `dsl/campaigns/shapes.memql` -- campaignFull, audienceFull, templateFull,
// recipientFull, deliveryFull, senderIdentityFull, emailRuleFull -- and every
// key here appears in one of them. `delivery.attempts` and
// `delivery.nextAttemptAt` are deliberately absent: they live only in
// `deliveryLedgerEntry`, which is the drain worker's projection, and
// `deliveriesForCampaign` (the read this app makes) binds `deliveryFull`.

export const CAMPAIGN_CONCEPT = "v1:campaigns:campaign";
export const AUDIENCE_CONCEPT = "v1:campaigns:audience";
export const TEMPLATE_CONCEPT = "v1:campaigns:template";
export const SENDER_IDENTITY_CONCEPT = "v1:campaigns:senderIdentity";
export const EMAIL_RULE_CONCEPT = "v1:campaigns:emailRule";
export const RECIPIENT_CONCEPT = "v1:campaigns:recipient";
export const DELIVERY_CONCEPT = "v1:campaigns:delivery";

// ---------------------------------------------------------------------------
// Campaigns
// ---------------------------------------------------------------------------

export interface CampaignRow {
  testSourceCampaignId: string;
  id: string;
  ownerUserId: string;
  name: string;
  audienceId: string;
  templateId: string;
  fromName: string;
  replyTo: string;
  scheduledAt: string;
  /** draft | scheduled | sending | paused | sent | cancelled | failed. */
  status: string;
  startedAt: string;
  completedAt: string;
  /** FROZEN AT SEND TIME by the preflight -- how many the run will work
   *  through. Zero on a draft, which is not the same as an empty audience. */
  recipientCount: number;
  sentCount: number;
  failedCount: number;
  skippedCount: number;
  /** The engine's own sentence. Rendered verbatim, never paraphrased. */
  lastError: string;
  accountId: string;
  senderIdentityId: string;
  trackOpens: boolean;
  trackClicks: boolean;
  createdAt: string;
}

/**
 * A numeric field that may be ABSENT.
 *
 * The SDK has no `rowNumber`, and a folded CDC event carries only what the
 * write touched -- so a campaign row that arrived from a progress update
 * carries counters and a row that arrived from a rename does not. Absent
 * reads as 0 here because these four fields are stamped at create
 * (`createCampaign` writes `sentCount: 0` and its siblings), so a campaign
 * with no counter is a campaign whose counter is genuinely zero.
 */
function numberOr(row: Row, key: string, fallback: number): number {
  const v = row[key];
  if (typeof v === "number" && Number.isFinite(v)) return v;
  // The wire renders an integer as a JSON number, but a value that made a
  // round trip through a string column is still a number a person wrote.
  if (typeof v === "string" && v.trim() !== "") {
    const parsed = Number(v);
    if (Number.isFinite(parsed)) return parsed;
  }
  return fallback;
}

function boolOrTrue(row: Row, key: string): boolean {
  const v = row[key];
  return typeof v === "boolean" ? v : true;
}

export function campaignFromRow(row: Row): CampaignRow {
  const flat = flatten(row);
  return {
    testSourceCampaignId: rowString(flat, "testSourceCampaignId"),
    id: rowString(flat, "id"),
    ownerUserId: rowString(flat, "ownerUserId"),
    name: rowString(flat, "name"),
    audienceId: rowString(flat, "audienceId"),
    templateId: rowString(flat, "templateId"),
    fromName: rowString(flat, "fromName"),
    replyTo: rowString(flat, "replyTo"),
    scheduledAt: rowString(flat, "scheduledAt"),
    // NOT defaulted to "draft". A row whose status the fold has not seen is a
    // row we do not know the status of, and rendering that as a draft would
    // offer a Start button over a send already in flight.
    status: rowString(flat, "status"),
    startedAt: rowString(flat, "startedAt"),
    completedAt: rowString(flat, "completedAt"),
    recipientCount: numberOr(flat, "recipientCount", 0),
    sentCount: numberOr(flat, "sentCount", 0),
    failedCount: numberOr(flat, "failedCount", 0),
    skippedCount: numberOr(flat, "skippedCount", 0),
    lastError: rowString(flat, "lastError"),
    accountId: rowString(flat, "accountId"),
    senderIdentityId: rowString(flat, "senderIdentityId"),
    // `?? true` on the concept, so absent means ON. `boolOr` from the kit
    // exists for exactly this and is spelled out here because the default is
    // the surprising half.
    trackOpens: boolOrTrue(flat, "trackOpens"),
    trackClicks: boolOrTrue(flat, "trackClicks"),
    createdAt: rowString(flat, "createdAt"),
  };
}

/**
 * What a PERSON would call a change to a campaign, for the arrival cue.
 *
 * ===========================================================================
 * THE COUNTERS ARE NOT NEWS, AND THIS IS THIS APP'S SHARPEST CASE OF IT
 * ===========================================================================
 * A HEARTBEAT IS NOT NEWS (clients/os/README.md). Fleet's case is a machine
 * that heartbeats every 15 seconds; the domains panel's is a sweep every two
 * minutes. This one is worse than both while it lasts: the drain worker writes
 * `sentCount` / `failedCount` / `skippedCount` on EVERY BATCH of a send, so
 * naming any of them here would ring every row in the list, several times a
 * second, for the entire duration of every send in the cluster -- the standing
 * badge this cue exists not to be, arriving exactly when somebody is watching.
 *
 * They still RE-RENDER live, and that is the whole point of the send bar: the
 * band fills under the person watching it. Re-rendering and ringing are
 * different statements, and the fingerprint is what separates them.
 *
 * `recipientCount` is out for the same reason -- it is frozen by the preflight
 * at the same moment the status flips to `sending`, so it would fire a second
 * cue for a change the status already announced.
 *
 * `lastError` IS in. It is not a counter and it does not move on a timer: it
 * changes when a send hits something, which is news by construction and the
 * one thing an operator most wants to be told about without looking.
 */
export function campaignFingerprint(campaign: CampaignRow): string {
  return [
    campaign.name,
    campaign.status,
    campaign.scheduledAt,
    campaign.audienceId,
    campaign.templateId,
    campaign.senderIdentityId,
    campaign.accountId,
    campaign.lastError,
  ].join("|");
}

/** Campaign statuses that are over. Nothing more will be written to a run in
 *  one of these, so the list can file them away without hiding work. */
const FINISHED_STATUSES = new Set(["sent", "cancelled", "failed"]);

export function campaignIsFinished(campaign: CampaignRow): boolean {
  return FINISHED_STATUSES.has(campaign.status);
}

/** Whether a send is currently moving. Drives the "still going" reading of
 *  the bar and the presence of Pause. */
export function campaignIsRunning(campaign: CampaignRow): boolean {
  return campaign.status === "sending";
}

/**
 * The name to show, never blank.
 *
 * A blank cell is indistinguishable from a cell that failed to render, and a
 * campaign mid-edit can arrive with anything. The id's tail is the fallback
 * because it is the one thing the row always has.
 */
export function campaignName(campaign: CampaignRow): string {
  return orUntitled(campaign.name, campaign.id, "campaign");
}

function orUntitled(name: string, id: string, noun: string): string {
  const trimmed = name.trim();
  if (trimmed !== "") return trimmed;
  const tail = id.split(":").pop() ?? "";
  return tail === "" ? `Untitled ${noun}` : `Untitled ${noun} (${tail})`;
}

// ---------------------------------------------------------------------------
// The send bar: the audience, divided by what happened to it
// ---------------------------------------------------------------------------

export type SendOutcome = "sent" | "skipped" | "failed" | "pending";

export interface SendSegment {
  key: SendOutcome;
  /** What this slice IS, in the reader's words. */
  label: string;
  count: number;
  /** Share of the audience, 0..1. Zero when the audience size is unknown. */
  share: number;
}

export interface SendBreakdown {
  /** The audience the run is working through. 0 before a send has started. */
  total: number;
  segments: SendSegment[];
  /** True when nothing has been counted yet -- a draft, or a send that has
   *  not had its first batch. The bar renders as an outline rather than as a
   *  band that is 100% "pending", which would claim a size it does not know. */
  empty: boolean;
}

/**
 * The bar's model: one audience partitioned by outcome.
 *
 * A BAR RATHER THAN FOUR STAT CARDS, and the difference is what the reader has
 * to do. Four numbers make a person add them up to learn the one thing they
 * came for -- how far through is this, and did it go well. One band divided
 * proportionally IS that answer, and it makes the SKIPPED slice visible, which
 * is the compliance-relevant number nobody goes looking for.
 *
 * PENDING IS DERIVED, NEVER READ. There is no `pendingCount` on the row, and
 * there should not be: pending is "the audience minus everything that has
 * happened", which is exactly the arithmetic the bar is drawing. Deriving it
 * also means the four slices always sum to the whole, so the band can never
 * render a gap that looks like a rendering fault.
 *
 * IT CLAMPS AT ZERO. A counter that ran past `recipientCount` -- a resumed
 * send whose roster shrank mid-run, say -- would otherwise produce a negative
 * pending slice and a band longer than its container. Clamping is the honest
 * repair: the outcome counts are observations and the total is a frozen
 * estimate, so when they disagree the observations win.
 */
export function sendBreakdown(campaign: CampaignRow): SendBreakdown {
  const sent = Math.max(0, campaign.sentCount);
  const skipped = Math.max(0, campaign.skippedCount);
  const failed = Math.max(0, campaign.failedCount);
  const done = sent + skipped + failed;
  // The total is the frozen roster size, or the work already done when that
  // is larger (or absent, on a campaign whose preflight has not run).
  const total = Math.max(campaign.recipientCount, done);
  const pending = Math.max(0, total - done);

  const share = (n: number) => (total > 0 ? n / total : 0);
  return {
    total,
    empty: total === 0,
    segments: [
      { key: "sent", label: "Sent", count: sent, share: share(sent) },
      { key: "skipped", label: "Skipped", count: skipped, share: share(skipped) },
      { key: "failed", label: "Failed", count: failed, share: share(failed) },
      { key: "pending", label: "Not yet sent", count: pending, share: share(pending) },
    ],
  };
}

/**
 * The bar, in words -- the `aria-label` a screen reader gets.
 *
 * A bar somebody cannot read is a bar that excluded them, and the picture's
 * whole content is proportion, which no list of numbers beside it conveys. So
 * the label states the figures AS a division of the audience, in the same
 * order the band draws them, and it names the slices with zero counts rather
 * than dropping them -- "no failures" is the reading somebody wants and an
 * omitted slice is silence about it.
 */
export function sendBreakdownLabel(breakdown: SendBreakdown): string {
  if (breakdown.empty) return "Nothing has been sent yet.";
  const parts = breakdown.segments.map((s) => `${s.count} ${s.label.toLowerCase()}`);
  return `${breakdown.total} recipients: ${parts.join(", ")}.`;
}

// ---------------------------------------------------------------------------
// Stats: the figures the server computed, and the ones it could not
// ---------------------------------------------------------------------------

/**
 * One engagement figure, which may honestly have no answer.
 *
 * ABSENT IS NOT ZERO. `campaignStats` reports a unique open/click count as
 * UNMEASURED when the bounded read behind it came back at its bound, and
 * reports no soft-bounce figure at all because nothing measures one per
 * campaign. Rendering either as `0` would be this window inventing a fact
 * about somebody's send -- and a zero open rate is a thing operators act on.
 */
export interface Figure {
  /** null = not measured. A number, including 0, is a measurement. */
  value: number | null;
  /** Why there is no number, in the surface's own voice. "" when there is. */
  absentBecause: string;
  /** This is a lower bound from a capped read, never an exact total. */
  atLeast?: boolean;
}

export const NOT_MEASURED: Figure = { value: null, absentBecause: "" };

export interface CampaignStats {
  pending: Figure;
  sent: Figure;
  failed: Figure;
  skipped: Figure;
  skippedSuppressed: Figure;
  skippedUnsubscribed: Figure;
  skippedOther: Figure;
  /** Provider reports; sent remains transport acceptance. */
  deliveryConfirmed: Figure;
  deliveryFailed: Figure;
  deliveryExpanded: Figure;
  deliveryUnconfirmed: Figure;
  hardBounces: Figure;
  /** Never measured: nothing counts a soft bounce per campaign. Always absent, with why. */
  softBounces: Figure;
  complaints: Figure;
  unsubscribes: Figure;
  opensTotal: Figure;
  opensUnique: Figure;
  clicksTotal: Figure;
  clicksUnique: Figure;
}

const UNIQUE_UNMEASURED =
  "Too many recorded events to count distinct recipients exactly, so this is left unstated rather than guessed.";
/** Why there is never a soft-bounce figure. Said once, where the breakdown is. */
export const SOFT_BOUNCE_ABSENT = "Soft bounces aren't counted per campaign.";

/**
 * Read a figure out of the stats payload.
 *
 * A KEY THAT IS ABSENT AND A KEY THAT IS null ARE THE SAME ANSWER, and both
 * are "not measured". That is deliberate: the server signals an unmeasured
 * unique count by omitting it, and a server that instead sent an explicit null
 * would mean the same thing. Anything that is not a finite number is not a
 * measurement.
 */
export function figureOf(payload: Row, key: string, absentBecause = ""): Figure {
  const v = payload[key];
  if (typeof v === "number" && Number.isFinite(v)) return { value: v, absentBecause: "" };
  if (typeof v === "string" && v.trim() !== "" && Number.isFinite(Number(v))) {
    return { value: Number(v), absentBecause: "" };
  }
  return { value: null, absentBecause };
}

/**
 * Project the `campaignStats` builtin's payload.
 *
 * Nested shapes are read through `flatten` plus dotted fallbacks, because a
 * builtin's reply is a payload rather than a shaped row: `opens.unique` and
 * `opensUnique` are both plausible spellings of one figure, and reading both
 * costs a line while guessing wrong costs the whole engagement panel. The
 * fallback order is nested first, because the builtin's own description names
 * `opens.unique`.
 *
 * THE ENGINE'S OWN NAMES FIRST (component/campaigns/stats.go): hard bounces
 * are `bounces.hard` and one-click unsubscribes are `unsubscribed`. Read under
 * other names, both came back absent on every campaign -- and the hard-bounce
 * line borrowed the soft-bounce sentence for its reason.
 */
/** The first of two spellings that carries a measurement. */
function either(first: Figure, second: Figure): Figure {
  return first.value !== null ? first : second;
}

export function statsFromPayload(row: Row): CampaignStats {
  const flat = flatten(row);
  const nested = (group: string, leaf: string, flatKey: string, absent = ""): Figure => {
    const bag = flat[group];
    if (bag && typeof bag === "object" && !Array.isArray(bag)) {
      const inner = figureOf(bag as Row, leaf, absent);
      if (inner.value !== null) return inner;
    }
    return figureOf(flat, flatKey, absent);
  };

  const stats: CampaignStats = {
    pending: figureOf(flat, "pending"),
    sent: figureOf(flat, "sent"),
    failed: figureOf(flat, "failed"),
    skipped: nested("skipped", "total", "skipped"),
    skippedSuppressed: nested("skipped", "suppressed", "skippedSuppressed"),
    skippedUnsubscribed: nested("skipped", "unsubscribed", "skippedUnsubscribed"),
    skippedOther: nested("skipped", "other", "skippedOther"),
    deliveryConfirmed: nested("delivery", "confirmed", "deliveryConfirmed"),
    deliveryFailed: nested("delivery", "failed", "deliveryFailed"),
    deliveryExpanded: nested("delivery", "expanded", "deliveryExpanded"),
    deliveryUnconfirmed: nested("delivery", "unconfirmed", "deliveryUnconfirmed", "Unconfirmed delivery could not be counted exactly."),
    hardBounces: nested("bounces", "hard", "hardBounces"),
    softBounces: { value: null, absentBecause: SOFT_BOUNCE_ABSENT },
    complaints: figureOf(flat, "complaints"),
    unsubscribes: either(figureOf(flat, "unsubscribed"), figureOf(flat, "unsubscribes")),
    opensTotal: nested("opens", "total", "opensTotal"),
    opensUnique: nested("opens", "unique", "opensUnique", UNIQUE_UNMEASURED),
    clicksTotal: nested("clicks", "total", "clicksTotal"),
    clicksUnique: nested("clicks", "unique", "clicksUnique", UNIQUE_UNMEASURED),
  };
  const bounds = flat.atLeast;
  if (bounds && typeof bounds === "object" && !Array.isArray(bounds)) {
    const keys: Partial<Record<keyof CampaignStats, string>> = { pending: "pending", sent: "sent", failed: "failed", skipped: "skipped.total", skippedSuppressed: "skipped.suppressed", skippedUnsubscribed: "skipped.unsubscribed", skippedOther: "skipped.other", deliveryConfirmed: "delivery.confirmed", deliveryFailed: "delivery.failed", deliveryExpanded: "delivery.expanded", hardBounces: "bounces.hard", complaints: "complaints", unsubscribes: "unsubscribed", opensTotal: "opens.total", opensUnique: "opens.unique", clicksTotal: "clicks.total", clicksUnique: "clicks.unique" };
    for (const [key, wire] of Object.entries(keys)) if ((bounds as Row)[wire] === true) stats[key as keyof CampaignStats].atLeast = true;
  }
  return stats;
}

/**
 * A rate expressed against what was accepted for sending, or null.
 *
 * OF SENT, NEVER OF THE AUDIENCE. An open rate over the whole roster
 * silently punishes a campaign for its suppressions, which is the opposite of
 * what suppressing was for. A zero denominator has no rate -- not 0%.
 */
export function rateOf(numerator: Figure, sent: number, denominatorAtLeast = false): number | null {
  if (numerator.value === null || numerator.atLeast || denominatorAtLeast || sent <= 0) return null;
  return numerator.value / sent;
}

/** A rate as a person reads one. Null renders as the em dash everywhere. */
export function formatRate(rate: number | null): string {
  if (rate === null) return "--";
  const pct = rate * 100;
  return pct >= 10 || pct === 0 ? `${Math.round(pct)}%` : `${pct.toFixed(1)}%`;
}

/** A measured figure, or the em dash. Never a zero standing in for silence. */
export function formatFigure(figure: Figure): string {
  return figure.value === null ? "--" : `${figure.atLeast ? "≥" : ""}${figure.value}`;
}

// ---------------------------------------------------------------------------
// Audiences
// ---------------------------------------------------------------------------

export interface AudienceRow {
  id: string;
  ownerUserId: string;
  name: string;
  description: string;
  /** active | archived. */
  status: string;
  accountId: string;
  /**
   * Whether somebody who joins waits, pending, until they confirm their address
   * (double opt-in). The engine adds the field with the automations work, so a
   * row that does not carry it is one that does not ask: absent reads as none.
   */
  confirmation: "none" | "required";
  createdAt: string;
}

export function audienceFromRow(row: Row): AudienceRow {
  const flat = flatten(row);
  return {
    id: rowString(flat, "id"),
    ownerUserId: rowString(flat, "ownerUserId"),
    name: rowString(flat, "name"),
    description: rowString(flat, "description"),
    status: rowString(flat, "status"),
    accountId: rowString(flat, "accountId"),
    confirmation: rowString(flat, "confirmation") === "required" ? "required" : "none",
    createdAt: rowString(flat, "createdAt"),
  };
}

export function audienceIsArchived(audience: AudienceRow): boolean {
  return audience.status === "archived";
}

export function audienceName(audience: AudienceRow): string {
  return orUntitled(audience.name, audience.id, "audience");
}

/** Every field a person edits. `v1:campaigns:audience` carries nothing the
 *  engine churns, so there is nothing here to leave out. */
export function audienceFingerprint(audience: AudienceRow): string {
  return [audience.name, audience.description, audience.status, audience.accountId, audience.confirmation].join("|");
}

// ---------------------------------------------------------------------------
// Testing settings
// ---------------------------------------------------------------------------

/** What an organization chose to send its campaign tests to (`campaignTestSettingsFull`). */
export interface TestSettingRow {
  id: string;
  accountId: string;
  /** "" is a setting too: none is chosen. */
  audienceId: string;
  /** The revision the engine checks a change against (`expectedRevision`). */
  createdAt: string;
}

export function testSettingFromRow(row: Row): TestSettingRow {
  const flat = flatten(row);
  return {
    id: rowString(flat, "id"),
    accountId: rowString(flat, "accountId"),
    audienceId: rowString(flat, "audienceId"),
    createdAt: rowString(flat, "createdAt"),
  };
}

/**
 * One organization's testing setting, out of the rows the app's one
 * testing-settings feed holds for every organization in view. Null when the
 * feed holds none for it -- which is "none chosen" or "not read", and the feed
 * (`settled`, `refusal`) is what says which. An organization of "" has none: a
 * campaign with no organization has nobody's setting to use.
 */
export function testSettingFor(rows: readonly Row[], accountId: string): TestSettingRow | null {
  if (accountId === "") return null;
  for (const row of rows) {
    const setting = testSettingFromRow(row);
    if (setting.accountId === accountId) return setting;
  }
  return null;
}

// ---------------------------------------------------------------------------
// Repeating campaigns
// ---------------------------------------------------------------------------

/**
 * A campaign that repeats: the series a draft sends from (`campaignSeriesFull`).
 *
 * Each occurrence is a campaign of its own, made from what the series CAPTURED
 * when it was last saved -- so editing the draft changes nothing that goes out
 * until the series is saved again, and cancelling the draft does not stop it.
 */
export interface SeriesRow {
  id: string;
  /** The draft the series was made from, and whose page carries its acts. */
  sourceCampaignId: string;
  accountId: string;
  name: string;
  intervalWeeks: number;
  /** IANA zone the cadence keeps its local day and time in. */
  timeZone: string;
  /** The first occurrence: what the cadence is counted from. */
  anchorAt: string;
  nextAt: string;
  /** active | paused | blocked. */
  status: string;
  lastCampaignId: string;
  lastOccurrenceAt: string;
  /** Why a blocked series stopped, in the engine's words. Rendered verbatim. */
  lastError: string;
  /** The revision a pause, a resume or a save is checked against (`expectedRevision`). */
  createdAt: string;
}

export function seriesFromRow(row: Row): SeriesRow {
  const flat = flatten(row);
  return {
    id: rowString(flat, "id"),
    sourceCampaignId: rowString(flat, "sourceCampaignId"),
    accountId: rowString(flat, "accountId"),
    name: rowString(flat, "name"),
    intervalWeeks: numberOr(flat, "intervalWeeks", 0),
    timeZone: rowString(flat, "timeZone"),
    anchorAt: rowString(flat, "anchorAt"),
    nextAt: rowString(flat, "nextAt"),
    status: rowString(flat, "status"),
    lastCampaignId: rowString(flat, "lastCampaignId"),
    lastOccurrenceAt: rowString(flat, "lastOccurrenceAt"),
    lastError: rowString(flat, "lastError"),
    createdAt: rowString(flat, "createdAt"),
  };
}

/** One campaign's series, out of every series the app's one series feed holds; null when it has none. */
export function seriesFor(rows: readonly Row[], campaignId: string): SeriesRow | null {
  if (campaignId === "") return null;
  for (const row of rows) {
    const series = seriesFromRow(row);
    if (series.sourceCampaignId === campaignId) return series;
  }
  return null;
}

// ---------------------------------------------------------------------------
// Recipients
// ---------------------------------------------------------------------------

export interface RecipientRow {
  id: string;
  audienceId: string;
  email: string;
  displayName: string;
  /** subscribed | unsubscribed | bounced | complained. */
  subscriptionStatus: string;
  unsubscribedAt: string;
  /** manual | import | signup. */
  source: string;
  /** Every spare CSV column, verbatim. The `{{fields.*}}` merge data. */
  fields: Record<string, string>;
  createdAt: string;
}

/**
 * The merge map, flattened to strings.
 *
 * A NON-STRING VALUE IS KEPT AND STRINGIFIED rather than dropped. The import
 * writes strings, but a row written by hand can carry anything, and a key that
 * exists on the recipient must appear in the merge-tag list -- the whole point
 * of that list is that it says what IS there.
 */
function fieldsOf(row: Row): Record<string, string> {
  const raw = row["fields"];
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return {};
  const out: Record<string, string> = {};
  for (const [key, value] of Object.entries(raw as Record<string, unknown>)) {
    if (value === null || value === undefined) continue;
    out[key] = typeof value === "string" ? value : String(value);
  }
  return out;
}

export function recipientFromRow(row: Row): RecipientRow {
  const flat = flatten(row);
  return {
    id: rowString(flat, "id"),
    audienceId: rowString(flat, "audienceId"),
    email: rowString(flat, "email"),
    displayName: rowString(flat, "displayName"),
    subscriptionStatus: rowString(flat, "subscriptionStatus"),
    unsubscribedAt: rowString(flat, "unsubscribedAt"),
    source: rowString(flat, "source"),
    fields: fieldsOf(flat),
    createdAt: rowString(flat, "createdAt"),
  };
}

export function recipientIsSendable(recipient: RecipientRow): boolean {
  return recipient.subscriptionStatus === "subscribed";
}

/**
 * How many of a roster a send would actually reach.
 *
 * The difference between this and the roster length IS the suppression rate,
 * which is the number an operator about to schedule wants, and it is one the
 * roster read already contains -- `recipientsForAudience` returns suppressed
 * rows deliberately so this subtraction is possible without a second read.
 */
export function sendableCount(recipients: readonly RecipientRow[]): number {
  return recipients.filter(recipientIsSendable).length;
}

// ---------------------------------------------------------------------------
// Templates
// ---------------------------------------------------------------------------

/**
 * A row of the templates list: a SUMMARY, as the engine's templateSummary shape
 * projects it. It has no body and no document -- those can weigh megabytes, a
 * query answer is one message and a list holds up to fifty, so a list that
 * carried them would outgrow the connection. A template is read whole by id
 * (templateById); nothing here is the content to edit or to send back.
 */
export interface TemplateRow {
  id: string;
  ownerUserId: string;
  name: string;
  subject: string;
  /** draft | ready | archived. */
  status: string;
  accountId: string;
  /** The layout the email was made from ("builtin:<name>" / "org:<id>"), or "". */
  layoutId: string;
  preheader: string;
  /** The Library file the template was published from, or "". */
  sourceArtifactId: string;
  /** The revision: it moves on every save. */
  createdAt: string;
}

export function templateFromRow(row: Row): TemplateRow {
  const flat = flatten(row);
  return {
    id: rowString(flat, "id"),
    ownerUserId: rowString(flat, "ownerUserId"),
    name: rowString(flat, "name"),
    subject: rowString(flat, "subject"),
    status: rowString(flat, "status"),
    accountId: rowString(flat, "accountId"),
    layoutId: rowString(flat, "layoutId"),
    preheader: rowString(flat, "preheader"),
    sourceArtifactId: rowString(flat, "sourceArtifactId"),
    createdAt: rowString(flat, "createdAt"),
  };
}

export function templateName(template: TemplateRow): string {
  return orUntitled(template.name, template.id, "template");
}

export function templateIsArchived(template: TemplateRow): boolean {
  return template.status === "archived";
}

/**
 * Whether a template is published: `ready` is the engine's word for it, and a
 * person reads Published. Asked here and nowhere else, so the app has one answer
 * to "can this be sent" rather than eight spellings of it.
 */
export function isPublished(template: TemplateRow): boolean {
  return template.status === "ready";
}

/**
 * A template read WHOLE, by id (`templateById`, the templateFull shape): the
 * summary a list holds, and what it holds -- its email document and the bodies a
 * send uses. This is the only place an OS surface gets a template's content
 * from; a list row has none to give (the light list, memql#6032).
 */
export interface TemplateRecord extends TemplateRow {
  /**
   * The email document the template was saved from (layout or custom mode), or
   * null for a template saved before documents existed.
   */
  document: Record<string, unknown> | null;
  textBody: string;
  htmlBody: string;
}

export function templateRecordFromRow(row: Row): TemplateRecord {
  const flat = flatten(row);
  const document = rowObject(flat, "document");
  return {
    ...templateFromRow(row),
    // An empty object is no document: a document always says at least its subject.
    document: document !== null && Object.keys(document).length > 0 ? document : null,
    textBody: rowString(flat, "textBody"),
    htmlBody: rowString(flat, "htmlBody"),
  };
}

/** A COPY EDIT IS IN. Somebody else fixing a typo in a template a campaign is
 *  about to send is precisely the news the cue is for, and no body field moves
 *  on a timer. The list carries no body, so the edit is told by the revision:
 *  every save writes a new version, and `createdAt` is the version's time. */
export function templateFingerprint(template: TemplateRow): string {
  return [
    template.name,
    template.subject,
    template.preheader,
    template.layoutId,
    template.status,
    template.accountId,
    template.createdAt,
  ].join("|");
}

// ---------------------------------------------------------------------------
// Layouts
// ---------------------------------------------------------------------------
//
// A LAYOUT IS A ROW OF THE `emailLayouts` BUILTIN (layouts plan Ruling 16), not
// a concept row: the four built-ins are Go data that ships with the renderer,
// and an organization's own layouts follow them. One reading per organization
// lists both, so a list of every organization's layouts sees each built-in once
// per organization -- `layoutsInView` says each once.
//
// THE ID IS OPAQUE. "builtin:<name>" and "org:<id>" are the server's, the same
// string a template carries as its `layoutId`; the OS matches them whole,
// displays them only through the name the reading gives, and never builds or
// splits one. `layoutId` is the bare row id the server hands beside it for an
// organization's layout, which is what `campaignSaveLayout` and
// `emailLayoutById` take.

/** One row of an organization's `emailLayouts` reading. */
export interface LayoutRow {
  /** Opaque: "builtin:<name>" or "org:<id>". Displayed through `name`, never built or split. */
  id: string;
  kind: "builtin" | "organization";
  name: string;
  description: string;
  /** The layout's email document; {} when the listing left it out (`documentOmitted`). */
  document: Record<string, unknown>;
  /**
   * The listing left this layout's document out to stay under its size; the
   * layout is read by id (`emailLayoutById`) before it is previewed.
   */
  documentOmitted: boolean;
  status: "active" | "archived";
  /** The organization whose reading returned it; "" for a built-in once `layoutsInView` has said it once. */
  accountId: string;
  /** Organization layouts only: the bare row id, for the calls that take one. */
  layoutId?: string;
  /** Organization layouts only: the revision, which moves on every save. */
  revision?: string;
  /** Organization layouts only: the Library file it was saved from, or "". */
  sourceArtifactId?: string;
}

export function layoutFromRow(row: Row, accountId: string): LayoutRow {
  const flat = flatten(row);
  const kind = rowString(flat, "kind") === "builtin" ? "builtin" : "organization";
  const layout: LayoutRow = {
    id: rowString(flat, "id"),
    kind,
    name: rowString(flat, "name"),
    description: rowString(flat, "description"),
    document: rowObject(flat, "document") ?? {},
    documentOmitted: flat["documentOmitted"] === true,
    status: rowString(flat, "status") === "archived" ? "archived" : "active",
    accountId,
  };
  if (kind === "organization") {
    layout.layoutId = rowString(flat, "layoutId");
    layout.revision = rowString(flat, "revision");
    layout.sourceArtifactId = rowString(flat, "sourceArtifactId");
  }
  return layout;
}

/** A layout's name as a person reads it. Never its id. */
export function layoutTitle(layout: LayoutRow): string {
  return layout.name.trim() || "Untitled layout";
}

export function layoutIsArchived(layout: LayoutRow): boolean {
  return layout.status === "archived";
}

/**
 * What the Layouts view lists: each built-in once, then every organization's
 * own layouts in the order their readings came, archived ones only when asked.
 *
 * A built-in is every organization's, so once said it belongs to none of them
 * (`accountId` ""). The copy kept is the first that carries its document, so a
 * reading that had to leave it out does not stand for one that did not.
 */
export function layoutsInView(readings: ReadonlyMap<string, readonly LayoutRow[]>, includeArchived: boolean): LayoutRow[] {
  const builtIns = new Map<string, LayoutRow>();
  const own: LayoutRow[] = [];
  for (const rows of readings.values()) {
    for (const layout of rows) {
      if (layout.id === "") continue;
      if (layout.kind === "builtin") {
        const held = builtIns.get(layout.id);
        if (held === undefined || (held.documentOmitted && !layout.documentOmitted)) builtIns.set(layout.id, { ...layout, accountId: "" });
      } else if (includeArchived || !layoutIsArchived(layout)) {
        own.push(layout);
      }
    }
  }
  return [...builtIns.values(), ...own];
}

/**
 * The name of the layout a template was made from, by the opaque id the
 * template carries. "" when no reading knows it -- and for a template made
 * without one, whose word ("Custom") is the caller's to say.
 */
export function layoutName(readings: ReadonlyMap<string, readonly LayoutRow[]>, layoutId: string): string {
  if (layoutId === "") return "";
  for (const rows of readings.values()) {
    const found = rows.find((layout) => layout.id === layoutId);
    if (found !== undefined) return layoutTitle(found);
  }
  return "";
}

/**
 * The same layout, read again: nothing a person sees and nothing a preview
 * renders has moved. An organization's layout says so by its revision; a
 * built-in, which has none, by its document.
 */
export function sameLayout(a: LayoutRow, b: LayoutRow): boolean {
  if (
    a.id !== b.id || a.kind !== b.kind || a.name !== b.name || a.description !== b.description || a.status !== b.status ||
    a.documentOmitted !== b.documentOmitted || a.accountId !== b.accountId || a.layoutId !== b.layoutId ||
    a.revision !== b.revision || a.sourceArtifactId !== b.sourceArtifactId
  ) {
    return false;
  }
  return a.kind === "organization" ? true : JSON.stringify(a.document) === JSON.stringify(b.document);
}

/**
 * An organization layout read whole, by id (`emailLayoutById`, the
 * emailLayoutFull shape): who saved it, and the revision a change of its
 * state is checked against.
 */
export interface LayoutRecord {
  /** The bare row id. */
  layoutId: string;
  ownerUserId: string;
  accountId: string;
  name: string;
  description: string;
  document: Record<string, unknown> | null;
  sourceArtifactId: string;
  status: string;
  /** The revision: it moves on every save, archive and restore. */
  createdAt: string;
}

export function layoutRecordFromRow(row: Row): LayoutRecord {
  const flat = flatten(row);
  return {
    layoutId: rowString(flat, "id"),
    ownerUserId: rowString(flat, "ownerUserId"),
    accountId: rowString(flat, "accountId"),
    name: rowString(flat, "name"),
    description: rowString(flat, "description"),
    document: rowObject(flat, "document"),
    sourceArtifactId: rowString(flat, "sourceArtifactId"),
    status: rowString(flat, "status"),
    createdAt: rowString(flat, "createdAt"),
  };
}

// ---------------------------------------------------------------------------
// A preview, as the engine's one renderer wrote it
// ---------------------------------------------------------------------------

/** Something that keeps an email from being sent as it is, in the engine's words. */
export interface RenderIssue {
  code: string;
  /** The block it is about, when it is about one. */
  block: string;
  message: string;
}

/** What `emailRender` answers: the email as it would be sent. */
export interface EmailRender {
  subject: string;
  html: string;
  text: string;
  issues: RenderIssue[];
}

export function renderFromRow(row: Row): EmailRender {
  const flat = flatten(row);
  const issues = Array.isArray(flat["issues"]) ? (flat["issues"] as unknown[]) : [];
  return {
    subject: rowString(flat, "subject"),
    html: rowString(flat, "html"),
    text: rowString(flat, "text"),
    issues: issues
      .filter((issue): issue is Row => issue !== null && typeof issue === "object" && !Array.isArray(issue))
      .map((issue) => ({ code: rowString(issue, "code"), block: rowString(issue, "block"), message: rowString(issue, "message") }))
      .filter((issue) => issue.message !== ""),
  };
}

// ---------------------------------------------------------------------------
// Sender identities
// ---------------------------------------------------------------------------

export interface SenderIdentityRow {
  id: string;
  ownerUserId: string;
  address: string;
  fromName: string;
  replyTo: string;
  accountId: string;
  /** active | disabled. */
  status: string;
  notes: string;
  createdAt: string;
}

export function senderIdentityFromRow(row: Row): SenderIdentityRow {
  const flat = flatten(row);
  return {
    id: rowString(flat, "id"),
    ownerUserId: rowString(flat, "ownerUserId"),
    address: rowString(flat, "address"),
    fromName: rowString(flat, "fromName"),
    replyTo: rowString(flat, "replyTo"),
    accountId: rowString(flat, "accountId"),
    status: rowString(flat, "status"),
    notes: rowString(flat, "notes"),
    createdAt: rowString(flat, "createdAt"),
  };
}

export function senderIsRetired(sender: SenderIdentityRow): boolean {
  return sender.status === "disabled";
}

/** The address IS the identity, so it leads. A row with neither address nor
 *  name is mid-write and gets its id's tail rather than a blank line. */
export function senderLabel(sender: SenderIdentityRow): string {
  const address = sender.address.trim();
  if (address !== "") return address;
  return orUntitled(sender.fromName, sender.id, "sender");
}

export function senderFingerprint(sender: SenderIdentityRow): string {
  return [
    sender.address,
    sender.fromName,
    sender.replyTo,
    sender.status,
    sender.accountId,
    sender.notes,
  ].join("|");
}

// ---------------------------------------------------------------------------
// Event-email rules
// ---------------------------------------------------------------------------

export type RecipientMode = "cluster_roles" | "audience" | "row_address";

export interface EmailRuleRow {
  id: string;
  ownerUserId: string;
  name: string;
  description: string;
  accountId: string;
  triggerConcept: string;
  /** created | updated. */
  eventKind: string;
  condition: string;
  templateId: string;
  recipientMode: string;
  recipientRoles: string[];
  audienceId: string;
  recipientField: string;
  senderIdentityId: string;
  /** draft | active | paused | failed. */
  status: string;
  bundleId: string;
  constructName: string;
  /** The engine's own sentence when generation, validation or activation
   *  refused -- or when the circuit breaker tripped. Rendered verbatim. */
  lastError: string;
  lastFiredAt: string;
  firedCount: number;
  createdAt: string;
}

export function emailRuleFromRow(row: Row): EmailRuleRow {
  const flat = flatten(row);
  return {
    id: rowString(flat, "id"),
    ownerUserId: rowString(flat, "ownerUserId"),
    name: rowString(flat, "name"),
    description: rowString(flat, "description"),
    accountId: rowString(flat, "accountId"),
    triggerConcept: rowString(flat, "triggerConcept"),
    eventKind: rowString(flat, "eventKind"),
    condition: rowString(flat, "condition"),
    templateId: rowString(flat, "templateId"),
    recipientMode: rowString(flat, "recipientMode"),
    recipientRoles: stringsOf(flat, "recipientRoles"),
    audienceId: rowString(flat, "audienceId"),
    recipientField: rowString(flat, "recipientField"),
    senderIdentityId: rowString(flat, "senderIdentityId"),
    status: rowString(flat, "status"),
    bundleId: rowString(flat, "bundleId"),
    constructName: rowString(flat, "constructName"),
    lastError: rowString(flat, "lastError"),
    lastFiredAt: rowString(flat, "lastFiredAt"),
    firedCount: numberOr(flat, "firedCount", 0),
    createdAt: rowString(flat, "createdAt"),
  };
}

export function ruleName(rule: EmailRuleRow): string {
  return orUntitled(rule.name, rule.id, "rule");
}

/**
 * `lastFiredAt` and `firedCount` are OUT, and the concept says so itself:
 * "A LIVENESS field: it moves on its own, so a surface that fingerprints it
 * for arrival cues turns the rules list into a strobe. Display it; do not ring
 * on it."
 *
 * `lastError` is IN for the same reason it is in the campaign's: a rule whose
 * circuit breaker just tripped is the single most useful thing this list can
 * announce, and it is not a field that moves on a clock.
 */
export function ruleFingerprint(rule: EmailRuleRow): string {
  return [
    rule.name,
    rule.description,
    rule.triggerConcept,
    rule.eventKind,
    rule.condition,
    rule.templateId,
    rule.recipientMode,
    rule.recipientRoles.join(","),
    rule.audienceId,
    rule.recipientField,
    rule.senderIdentityId,
    rule.status,
    rule.lastError,
  ].join("|");
}

/**
 * A concept id in the reader's words: "user" out of "v1:identity:user".
 *
 * The bare entity, because that is the noun in the sentence -- "when a USER is
 * created". The full id stays on screen beside it in the data voice, so
 * somebody who needs to know which `user` (there is only one, but a product
 * bundle can add another entity with a familiar name) can see it.
 */
export function conceptEntity(conceptId: string): string {
  const parts = conceptId.split(":");
  return parts.length >= 3 ? parts.slice(2).join(":") : conceptId;
}

/** The two role-facing readings of a recipient mode, in one place so the
 *  builder and the list cannot disagree about what a rule does. */
export const RECIPIENT_MODES: {
  value: RecipientMode;
  /** The question's answer, as a person would say it. */
  label: string;
  /** What choosing it MEANS for the mail -- an effect, never a lane name. */
  effect: string;
}[] = [
  {
    value: "cluster_roles",
    label: "People in this cluster",
    effect:
      "Goes out as ordinary internal mail: no unsubscribe footer, and the do-not-mail list is not consulted. Telling your own people something happened is not marketing.",
  },
  {
    value: "audience",
    label: "Everyone in an audience",
    effect:
      "Goes out the way a campaign does: the do-not-mail list is checked before each message, an unsubscribe link is attached, and every outcome is recorded.",
  },
  {
    value: "row_address",
    label: "An address on the row that fired",
    effect:
      "Goes out the way a campaign does -- do-not-mail checked, unsubscribe attached, outcome recorded -- to whichever address the triggering row carries.",
  },
];

export function recipientModeLabel(mode: string): string {
  return RECIPIENT_MODES.find((m) => m.value === mode)?.label ?? mode;
}

/** One run of a rule's sentence. `code` marks something the person TYPED --
 *  the condition, the address field -- which is shown in the code face. */
export interface SentencePart {
  text: string;
  code?: true;
}

/**
 * A rule as one sentence, for the list line and the detail's headline.
 *
 * The list reads the same way the builder does, deliberately: somebody who
 * built a rule by filling in a sentence should recognise it in the list
 * without translating. Missing pieces render as their placeholder rather than
 * collapsing the sentence -- a half-built draft is a real state and it should
 * read as one.
 *
 * THE CONDITION IS QUOTED, NEVER REWORDED. It is the one expression a person
 * types in the OS, so it is shown the way it was typed, in the code face --
 * `row.role == "admin"`, never "its role is admin". Rewording it would be a
 * second copy of the grammar, and that copy would drift from the one that
 * runs. A legacy `payload.` condition, which the engine still converts, is
 * shown as stored.
 *
 * THE CONDITION COMES LAST, after the recipients: that is where the builder
 * asks for it, and a long condition in the middle would hold "When a user
 * changes" open across a line of code. The list line is cut to one line, so
 * last is also what keeps the template and the recipients readable there.
 */
export function ruleSentenceParts(
  rule: EmailRuleRow,
  names: { template: string; audience: string },
): SentencePart[] {
  const subject = rule.triggerConcept === "" ? "something" : `a ${conceptEntity(rule.triggerConcept)}`;
  const verb = rule.eventKind === "updated" ? "changes" : "is created";
  const template = names.template === "" ? "a template" : names.template;
  let who: SentencePart[];
  switch (rule.recipientMode) {
    case "cluster_roles":
      who = [
        {
          text:
            rule.recipientRoles.length === 0
              ? "the cluster owner"
              : rule.recipientRoles.join(" and ") + " in this cluster",
        },
      ];
      break;
    case "audience":
      who = [{ text: names.audience === "" ? "an audience" : `everyone in ${names.audience}` }];
      break;
    case "row_address":
      who =
        rule.recipientField === ""
          ? [{ text: "an address on the row" }]
          : [{ text: "the address in " }, { text: rule.recipientField, code: true }];
      break;
    default:
      who = [{ text: "somebody" }];
  }
  const condition = rule.condition.trim();
  const when: SentencePart[] =
    condition === "" ? [] : [{ text: ", but only when " }, { text: condition, code: true }];
  return [{ text: `When ${subject} ${verb}, email ${template} to ` }, ...who, ...when, { text: "." }];
}

/** The same sentence as plain text: the list line's full reading, on hover,
 *  for when one line cuts it short. */
export function ruleSentence(
  rule: EmailRuleRow,
  names: { template: string; audience: string },
): string {
  return ruleSentenceParts(rule, names)
    .map((part) => part.text)
    .join("");
}

/**
 * What `row` is in a rule's condition, in the words of the rule's own
 * sentence: "the user that changed".
 *
 * THE NOUN IS THE ONE THE BUILDER'S SENTENCE CHOSE, so the caption under the
 * condition points at the thing named just above it rather than at an
 * abstraction the person has to map back. Before a concept is chosen it is
 * "the record".
 */
export function conditionRowIs(rule: { triggerConcept: string; eventKind: string }): string {
  const noun = rule.triggerConcept === "" ? "record" : conceptEntity(rule.triggerConcept);
  return rule.eventKind === "updated" ? `the ${noun} that changed` : `the ${noun} that was created`;
}

// ---------------------------------------------------------------------------
// Deliveries
// ---------------------------------------------------------------------------

export interface DeliveryRow {
  id: string;
  campaignId: string;
  recipientId: string;
  email: string;
  /** pending | sent | failed | skipped. */
  status: string;
  outboundRequestId: string;
  /** Reported by the sending provider, never inferred from transport acceptance. */
  providerStatus: string;
  providerAt: string;
  skipReason: string;
  lastError: string;
  sentAt: string;
  createdAt: string;
}

export function deliveryFromRow(row: Row): DeliveryRow {
  const flat = flatten(row);
  return {
    id: rowString(flat, "id"),
    campaignId: rowString(flat, "campaignId"),
    recipientId: rowString(flat, "recipientId"),
    email: rowString(flat, "email"),
    status: rowString(flat, "status"),
    outboundRequestId: rowString(flat, "outboundRequestId"),
    providerStatus: rowString(flat, "providerStatus"),
    providerAt: rowString(flat, "providerAt"),
    skipReason: rowString(flat, "skipReason"),
    lastError: rowString(flat, "lastError"),
    sentAt: rowString(flat, "sentAt"),
    createdAt: rowString(flat, "createdAt"),
  };
}

/** A provider report is about delivery, separately from the send's transport status. */
export function providerDeliverySentence(status: string): string {
  switch (status) {
    case "delivered": return "Confirmed by recipient server";
    case "bounced": return "Delivery bounced";
    case "suppressed": return "Delivery suppressed";
    case "quarantined": return "Delivery quarantined";
    case "filtered-spam": return "Filtered as spam";
    case "expanded": return "Expanded to a distribution list";
    case "failed": return "Delivery failed";
    case "": return "Delivery unconfirmed";
    default: return `Delivery report: ${status}`;
  }
}

/**
 * Why one delivery was skipped, in the reader's words.
 *
 * AN UNRECOGNISED REASON KEEPS ITS OWN TEXT. The field carries either a
 * suppression reason or the recipient's own `subscriptionStatus`, and both
 * sets can grow; inventing a friendly sentence for a value this build does not
 * know is how a real cause gets mistaken for a familiar one.
 */
export function skipReasonSentence(reason: string): string {
  switch (reason) {
    case "unsubscribed":
      return "They unsubscribed";
    case "hard_bounce":
    case "bounced":
      return "The address bounced";
    case "complaint":
    case "complained":
      return "They reported a message as spam";
    case "manual":
      return "Suppressed by hand";
    case "":
      return "Skipped";
    default:
      return reason;
  }
}

// ---------------------------------------------------------------------------
// The cluster-wide kill switch for authored automations
// ---------------------------------------------------------------------------

/**
 * What `v1:identity:clusterSettings.authoredAutomationsEnabled` says.
 *
 * THREE ANSWERS, AND THE THIRD IS THE ONE THAT MATTERS. "unknown" is not a
 * degenerate case to collapse into one of the other two: the row is a
 * singleton a fresh cluster has never written, the concept declares no authz
 * tier today (so a later one would turn every read into zero rows rather than
 * an error, memql#4309), and a read can simply fail. Every one of those looks
 * identical from here and NONE of them means the switch is off.
 */
export type AuthoredAutomationsState = "running" | "halted" | "unknown";

/**
 * Read the switch out of a `clusterSettingsCurrent` row.
 *
 * ABSENT IS NOT FALSE, and getting that backwards is the whole risk of this
 * surface. `boolOr` exists precisely because the SDK's `rowBool` answers
 * `false` for a missing key, which would make a fresh cluster -- or a shape
 * that stops projecting the field -- render "every rule here is halted" on a
 * cluster where nothing is wrong. The fallback is `true`, which is both the
 * concept's own `@default("true")` and what the engine's own gate does with an
 * absent row ("a fresh cluster has authored automations on",
 * `app/engine_authored.go`).
 *
 * A NULL ROW IS "unknown" RATHER THAN "running", because the two are different
 * claims: "the cluster told us the switch is on" and "we did not get an
 * answer". The surface says nothing for either -- but only one of them is
 * something it could ever be asked to explain.
 */
export function authoredAutomationsFrom(row: Row | null): AuthoredAutomationsState {
  if (row === null) return "unknown";
  const flat = flatten(row);
  if (typeof flat["authoredAutomationsEnabled"] !== "boolean") return "unknown";
  return boolOr(flat, "authoredAutomationsEnabled", true) ? "running" : "halted";
}

/**
 * Why a rule is not running, when its row says `paused`.
 *
 * TWO THINGS WEAR THE SAME STATUS, and only one of them is somebody's
 * decision. `setEmailRuleStatus` is the operator's stop button and leaves
 * `lastError` alone; a run that FAILED writes the engine's sentence there
 * (`recordEmailRuleFiring`), and enough consecutive failures trip the
 * per-automation circuit breaker, which stops the automation without anybody
 * asking. Saying "you paused this" over the second case throws away the only
 * diagnostic there is.
 *
 * So the reading is made from the EVIDENCE rather than from a mechanism this
 * window cannot observe: a paused rule carrying a run failure is reported as
 * paused WITH that failure, and the copy does not claim who stopped it. That
 * is the honest form -- the row records what happened, not who did it.
 */
export type PauseReading = "operator" | "after_failure";

export function pauseReading(rule: EmailRuleRow): PauseReading {
  return rule.lastError.trim() === "" ? "operator" : "after_failure";
}

// ---------------------------------------------------------------------------
// The projections every page that picks or names a record reads
// ---------------------------------------------------------------------------
//
// A feed holds RAW wire rows (its fold upserts an event's payload as it came),
// so a page that offers an audience, a template or a sender projects the rows
// it was handed. These live here, beside the projections they apply, because
// the campaigns list, the campaign page, Add a campaign and the rules builder
// all read them -- a projection that lived in one section was a section the
// others had to import. `useProjected` is the one hook in this file: a memo
// around a pure projection, which is still checked without React.

export const audienceProjection = (rows: readonly Row[]): AudienceRow[] =>
  rows.map((r) => audienceFromRow(r)).filter((a) => a.id !== "");
export const templateProjection = (rows: readonly Row[]): TemplateRow[] =>
  rows.map((r) => templateFromRow(r)).filter((t) => t.id !== "");
export const senderProjection = (rows: readonly Row[]): SenderIdentityRow[] =>
  rows.map((r) => senderIdentityFromRow(r)).filter((s) => s.id !== "");

/** Memoise a projection on the SNAPSHOT ARRAY's identity, which the collection
 *  already holds stable until something actually changes. */
export function useProjected<T>(rows: readonly Row[], project: (rows: readonly Row[]) => T[]): T[] {
  // eslint-disable-next-line react-hooks/exhaustive-deps
  return useMemo(() => project(rows), [rows]);
}

export function nameOfAudience(audiences: AudienceRow[], id: string): string {
  return audiences.find((a) => a.id === id)?.name ?? "";
}
export function nameOfTemplate(templates: TemplateRow[], id: string): string {
  return templates.find((t) => t.id === id)?.name ?? "";
}
export function labelOfSender(senders: SenderIdentityRow[], id: string): string {
  return senders.find((s) => s.id === id)?.address ?? "";
}

export interface JourneyRow {
  id: string; accountId: string; ownerUserId: string; name: string; description: string;
  trigger: Trigger; steps: FlowStep[]; status: string; activatedAt: string; lastError: string; createdAt: string;
}
export function journeyFromRow(row: Row): JourneyRow {
  const flat = flatten(row);
  return { id: rowString(flat, "id"), accountId: rowString(flat, "accountId"), ownerUserId: rowString(flat, "ownerUserId"), name: rowString(flat, "name"), description: rowString(flat, "description"), trigger: (flat.trigger && typeof flat.trigger === "object" ? flat.trigger : { kind: "joined", audienceId: "" }) as Trigger, steps: Array.isArray(flat.steps) ? flat.steps as FlowStep[] : [], status: rowString(flat, "status"), activatedAt: rowString(flat, "activatedAt"), lastError: rowString(flat, "lastError"), createdAt: rowString(flat, "createdAt") };
}
export function journeyFingerprint(journey: JourneyRow): string { return JSON.stringify([journey.name, journey.description, journey.status, journey.trigger, journey.steps, journey.lastError]); }
