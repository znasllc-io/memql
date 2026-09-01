import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { boolOr, flatten, stringsOf } from "../../kit/rows";

// The wire rows the Campaigns app renders, projected into the shapes its
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
}

export const NOT_MEASURED: Figure = { value: null, absentBecause: "" };

export interface CampaignStats {
  recipients: Figure;
  pending: Figure;
  sent: Figure;
  failed: Figure;
  skipped: Figure;
  skippedSuppressed: Figure;
  skippedUnsubscribed: Figure;
  skippedOther: Figure;
  hardBounces: Figure;
  complaints: Figure;
  unsubscribes: Figure;
  opensTotal: Figure;
  opensUnique: Figure;
  clicksTotal: Figure;
  clicksUnique: Figure;
}

const UNIQUE_UNMEASURED =
  "Too many recorded events to count distinct recipients exactly, so this is left unstated rather than guessed.";
const SOFT_BOUNCE_ABSENT = "Nothing measures soft bounces per campaign, so there is no figure here.";

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
 */
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

  return {
    recipients: figureOf(flat, "recipients"),
    pending: figureOf(flat, "pending"),
    sent: figureOf(flat, "sent"),
    failed: figureOf(flat, "failed"),
    skipped: nested("skipped", "total", "skipped"),
    skippedSuppressed: nested("skipped", "suppressed", "skippedSuppressed"),
    skippedUnsubscribed: nested("skipped", "unsubscribed", "skippedUnsubscribed"),
    skippedOther: nested("skipped", "other", "skippedOther"),
    hardBounces: figureOf(flat, "hardBounces", SOFT_BOUNCE_ABSENT),
    complaints: figureOf(flat, "complaints"),
    unsubscribes: figureOf(flat, "unsubscribes"),
    opensTotal: nested("opens", "total", "opensTotal"),
    opensUnique: nested("opens", "unique", "opensUnique", UNIQUE_UNMEASURED),
    clicksTotal: nested("clicks", "total", "clicksTotal"),
    clicksUnique: nested("clicks", "unique", "clicksUnique", UNIQUE_UNMEASURED),
  };
}

/**
 * A rate expressed against what was actually delivered, or null.
 *
 * OF DELIVERED, NEVER OF THE AUDIENCE. An open rate over the whole roster
 * silently punishes a campaign for its suppressions, which is the opposite of
 * what suppressing was for. A zero denominator has no rate -- not 0%.
 */
export function rateOf(numerator: Figure, delivered: number): number | null {
  if (numerator.value === null || delivered <= 0) return null;
  return numerator.value / delivered;
}

/** A rate as a person reads one. Null renders as the em dash everywhere. */
export function formatRate(rate: number | null): string {
  if (rate === null) return "--";
  const pct = rate * 100;
  return pct >= 10 || pct === 0 ? `${Math.round(pct)}%` : `${pct.toFixed(1)}%`;
}

/** A measured figure, or the em dash. Never a zero standing in for silence. */
export function formatFigure(figure: Figure): string {
  return figure.value === null ? "--" : String(figure.value);
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
  return [audience.name, audience.description, audience.status, audience.accountId].join("|");
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

export interface TemplateRow {
  id: string;
  ownerUserId: string;
  name: string;
  subject: string;
  textBody: string;
  htmlBody: string;
  /** draft | ready | archived. */
  status: string;
  accountId: string;
  createdAt: string;
}

export function templateFromRow(row: Row): TemplateRow {
  const flat = flatten(row);
  return {
    id: rowString(flat, "id"),
    ownerUserId: rowString(flat, "ownerUserId"),
    name: rowString(flat, "name"),
    subject: rowString(flat, "subject"),
    textBody: rowString(flat, "textBody"),
    htmlBody: rowString(flat, "htmlBody"),
    status: rowString(flat, "status"),
    accountId: rowString(flat, "accountId"),
    createdAt: rowString(flat, "createdAt"),
  };
}

export function templateName(template: TemplateRow): string {
  return orUntitled(template.name, template.id, "template");
}

export function templateIsArchived(template: TemplateRow): boolean {
  return template.status === "archived";
}

/** THE BODIES ARE IN. A copy edit is the change this list exists to announce
 *  -- somebody else fixing a typo in a template a campaign is about to send is
 *  precisely the news the cue is for -- and no body field moves on a timer. */
export function templateFingerprint(template: TemplateRow): string {
  return [
    template.name,
    template.subject,
    template.textBody,
    template.htmlBody,
    template.status,
    template.accountId,
  ].join("|");
}

// ---------------------------------------------------------------------------
// Merge tags
// ---------------------------------------------------------------------------

/**
 * The closed set of tags that are not per-recipient data.
 *
 * CLOSED, and the renderer is a `strings.NewReplacer` over exactly these plus
 * the recipient's own `fields` keys -- a value is substituted and never
 * evaluated. So this list is not documentation that can go stale: a tag
 * outside it renders as its own literal text into somebody's inbox.
 */
export const BASE_MERGE_TAGS = [
  "{{displayName}}",
  "{{email}}",
  "{{campaignName}}",
  "{{accountName}}",
] as const;

export interface MergeTag {
  /** The tag exactly as it is typed into a body. */
  tag: string;
  /** What it resolves to for the sampled recipient, "" when nothing sampled. */
  preview: string;
  /** True for `{{fields.*}}`, which exist only because an import carried the
   *  column. That distinction is how somebody discovers `fields.*` at all. */
  fromImport: boolean;
}

export interface MergeSample {
  recipient: RecipientRow | null;
  campaignName: string;
  accountName: string;
}

/**
 * The tags available here, each showing what it renders to.
 *
 * THIS IS DOCUMENTATION THAT CANNOT GO STALE, which is the reason it is built
 * from a real recipient rather than written down. `{{fields.company}}` exists
 * because somebody's CSV had a `company` column; no list in this repo could
 * know that, and an operator has no other way to learn it. Showing the
 * resolved value beside the tag also catches the case a spelling check cannot:
 * a column that is present but empty for the person you sampled.
 *
 * A tag with no sample keeps its place and shows no preview. The tag is still
 * insertable -- the value simply is not known here yet, which is different
 * from the tag not existing.
 */
export function mergeTagsFor(sample: MergeSample): MergeTag[] {
  const recipient = sample.recipient;
  const base: MergeTag[] = [
    {
      tag: "{{displayName}}",
      preview: recipient?.displayName ?? "",
      fromImport: false,
    },
    { tag: "{{email}}", preview: recipient?.email ?? "", fromImport: false },
    { tag: "{{campaignName}}", preview: sample.campaignName, fromImport: false },
    { tag: "{{accountName}}", preview: sample.accountName, fromImport: false },
  ];
  const fields = Object.keys(recipient?.fields ?? {})
    .sort((a, b) => a.localeCompare(b))
    .map((key) => ({
      tag: `{{fields.${key}}}`,
      preview: recipient?.fields[key] ?? "",
      fromImport: true,
    }));
  return [...base, ...fields];
}

/**
 * Insert `tag` into `body` at the cursor, returning the new body and where
 * the cursor should land.
 *
 * THE CURSOR GOES AFTER THE TAG, not to the end and not back to the start. A
 * person clicking a chip is mid-sentence, and either alternative makes the
 * next keystroke land somewhere they were not.
 *
 * An out-of-range selection (a field that has never been focused reports 0,
 * and a stale one can report past the end) is clamped rather than refused:
 * inserting at the end is a reasonable answer to "we do not know where you
 * were", and throwing away the click is not.
 */
export function insertAt(body: string, tag: string, start: number, end: number): {
  body: string;
  cursor: number;
} {
  const from = Math.min(Math.max(0, start), body.length);
  const to = Math.min(Math.max(from, end), body.length);
  const next = body.slice(0, from) + tag + body.slice(to);
  return { body: next, cursor: from + tag.length };
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

/**
 * A rule as one sentence, for the list line.
 *
 * The list reads the same way the builder does, deliberately: somebody who
 * built a rule by filling in a sentence should recognise it in the list
 * without translating. Missing pieces render as their placeholder rather than
 * collapsing the sentence -- a half-built draft is a real state and it should
 * read as one.
 */
export function ruleSentence(
  rule: EmailRuleRow,
  names: { template: string; audience: string },
): string {
  const subject = rule.triggerConcept === "" ? "something" : `a ${conceptEntity(rule.triggerConcept)}`;
  const verb = rule.eventKind === "updated" ? "changes" : "is created";
  const template = names.template === "" ? "a template" : names.template;
  let who: string;
  switch (rule.recipientMode) {
    case "cluster_roles":
      who =
        rule.recipientRoles.length === 0
          ? "the cluster owner"
          : rule.recipientRoles.join(" and ") + " in this cluster";
      break;
    case "audience":
      who = names.audience === "" ? "an audience" : `everyone in ${names.audience}`;
      break;
    case "row_address":
      who =
        rule.recipientField === ""
          ? "an address on the row"
          : `the address in ${rule.recipientField}`;
      break;
    default:
      who = "somebody";
  }
  const when = rule.condition.trim() === "" ? "" : `, but only when ${rule.condition.trim()}`;
  return `When ${subject} ${verb}${when}, email ${template} to ${who}.`;
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
    skipReason: rowString(flat, "skipReason"),
    lastError: rowString(flat, "lastError"),
    sentAt: rowString(flat, "sentAt"),
    createdAt: rowString(flat, "createdAt"),
  };
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
// The email integration's self-report
// ---------------------------------------------------------------------------

export interface EmailReadiness {
  /** True only when the report SAYS it is unconfigured. Silence is not a
   *  refusal: an integration that publishes no self-report answers "unknown",
   *  and warning on that would put a permanent banner on a healthy cluster. */
  needsConfiguration: boolean;
  /** The report's own detail sentence, verbatim. "" when it gave none. */
  detail: string;
  /** graph | smtp | log | "". `log` is the state worth naming: mail "sends"
   *  and lands nowhere. */
  mode: string;
}

export const EMAIL_UNKNOWN: EmailReadiness = {
  needsConfiguration: false,
  detail: "",
  mode: "",
};

/**
 * Read the email lane's line out of an `integrationStatus` payload.
 *
 * TWO ANSWERS MEAN "not ready", and they are different failures: `configured:
 * "no"` is missing settings, and `health: "degraded"` is the log-only sender
 * -- running, answering, and delivering nothing. The second is the one that
 * looks fine from every other angle, which is why it is checked here at all.
 */
export function emailReadinessFrom(payload: Row): EmailReadiness {
  const flat = flatten(payload);
  const list = flat["integrations"];
  if (!Array.isArray(list)) return EMAIL_UNKNOWN;
  const email = list.find(
    (entry): entry is Row =>
      !!entry && typeof entry === "object" && (entry as Row)["name"] === "email",
  );
  if (!email) return EMAIL_UNKNOWN;
  const configured = rowString(email, "configured");
  const health = rowString(email, "health");
  return {
    needsConfiguration: configured === "no" || health === "degraded",
    detail: rowString(email, "detail"),
    mode: rowString(email, "mode"),
  };
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
