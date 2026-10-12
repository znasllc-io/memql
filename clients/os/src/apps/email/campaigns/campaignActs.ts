import type { ActionBarTone } from "../../../kit/ActionBar";
import type { Figure } from "../../../kit/measure";
import type { ActSpec } from "../acts";
import { campaignName, templateName, type CampaignRow, type SeriesRow, type TemplateRow } from "../rows";
import { browserZone, momentWords, whenWords } from "../when";
import { WORDS } from "../words";
import type { When } from "./wizardModel";

// ONE CAMPAIGN'S BAR, AS A PURE TABLE (memql#6033, Ruling 7).
//
// A campaign's page carries one ActionBar, and what it says and offers is a
// function of the campaign, the series it may repeat from, and what is known
// about sending it -- so "an act that is not legal is ABSENT, never disabled" is
// a property of a list that does not hold the act, asserted without a DOM.
//
//   draft, no time             Edit, Send a test, Send now
//   draft, a time to come      Edit, Send a test, Schedule
//   draft, something missing   Edit, Send a test        (the bar names it)
//   draft a series repeats     Edit, Send a test, Pause | Resume
//   ... and it stopped         Edit, Resume             (the series' sentence)
//   scheduled                  Cancel (asks), Send a test, Send now
//   sending                    Send a test, Pause
//   paused                     Cancel (asks), Send a test, Resume (sending ready)
//   sent, cancelled, failed    Send a test, Duplicate   (+ the series' act)
//
// `Send a test` is there whenever a test can go somewhere (Ruling 10), in every
// state the engine accepts one. WHAT IS STILL BEING READ OFFERS NOTHING: while
// the template, the series or sending readiness is unknown the bar says nothing
// about it and offers no act that depends on it -- a step knows before it offers.

export interface CampaignFacts {
  campaign: CampaignRow;
  /** The series this campaign is the source of, or null for none. */
  series: SeriesRow | null;
  /** The campaign's template, as the organization's templates hold it; absent when it is not there. */
  template?: TemplateRow;
  sendingReady: boolean;
  /** The engine's reason sending is not ready, or the failed check's own error. Verbatim. */
  sendingReason: string;
  /** A test has somewhere to go and can go there now. */
  canTest: boolean;
  organizationName: string;
  now: Date;

  // ---- What is still being read. Each one left out is known. ----
  /** False while sending readiness is being read. */
  sendingKnown?: boolean;
  /** The readiness check itself failed: nothing is known about sending either way. */
  sendingFailed?: boolean;
  /** False while the templates are being read, or when the read was refused. */
  templateKnown?: boolean;
  /** False while this campaign's series is being read, or when the read was refused. */
  seriesKnown?: boolean;
  /** How many people the audience reaches now. A measured zero stops a send; an unread count does not. */
  subscribers?: Figure;
  /** The audience's name, for the sentence that says it reaches nobody. */
  audienceName?: string;
  /** The zone a time is read on: this browser's. */
  zone?: string;
}

/** What the bar says: the state in a word, what it means, and the dot's tone. */
export interface CampaignReading {
  word: string;
  detail: string;
  tone: ActionBarTone;
}

const EDIT: ActSpec = { id: "edit", label: "Edit", text: true };
const TEST: ActSpec = { id: "test", label: "Send a test", text: true };
const SEND: ActSpec = { id: "send", label: "Send now", tone: "primary" };
const SCHEDULE: ActSpec = { id: "schedule", label: "Schedule", tone: "primary" };
const PAUSE: ActSpec = { id: "pause", label: "Pause", tone: "primary" };
const RESUME: ActSpec = { id: "resume", label: "Resume", tone: "primary" };
const CANCEL: ActSpec = { id: "cancel", label: "Cancel", text: true };
const DUPLICATE: ActSpec = { id: "duplicate", label: "Duplicate", text: true };

/**
 * What stops the campaign going out, in the order a person would fix it: no
 * organization, a template that cannot be sent, sending, an audience nobody is
 * subscribed to. "" when nothing does; null while something before the first
 * blocker is still being read, because then the first one is not known yet.
 */
function blocker(f: CampaignFacts): string | null {
  const { campaign } = f;
  if (campaign.accountId === "") return "Edit it to choose an organization.";
  if (f.templateKnown === false) return null;
  const template = f.template;
  if (template === undefined || template.accountId !== campaign.accountId || template.status === "archived") return "Choose a published template.";
  if (template.status !== "ready") return `Publish “${templateName(template)}” before sending.`;
  const sending = sendingBlocker(f);
  if (sending !== "") return sending;
  if (f.subscribers?.kind === "measured" && f.subscribers.value === 0) return `${f.audienceName || "Its audience"} has no subscribers.`;
  return "";
}

/** Sending alone: unknown (null), a sentence, or "" when it is ready. */
function sendingBlocker(f: CampaignFacts): string | null {
  if (f.sendingKnown === false) return null;
  if (f.sendingFailed === true) return WORDS.sendingNotChecked(f.organizationName);
  if (!f.sendingReady) return WORDS.sendingNotSetUp(f.organizationName);
  return "";
}

/** Where a draft's saved time stands: none, still to come, or gone. */
function savedTime(f: CampaignFacts): "none" | "future" | "past" {
  const at = Date.parse(f.campaign.scheduledAt);
  if (f.campaign.scheduledAt === "" || Number.isNaN(at)) return "none";
  return at > f.now.getTime() ? "future" : "past";
}

/** The series' own act: Pause while it repeats, Resume once it is paused or stopped. */
function seriesAct(series: SeriesRow | null): ActSpec[] {
  if (series === null) return [];
  if (series.status === "active") return [PAUSE];
  if (series.status === "paused" || series.status === "blocked") return [RESUME];
  return [];
}

export function campaignActs(f: CampaignFacts): ActSpec[] {
  const { campaign, series } = f;
  const test = f.canTest ? [TEST] : [];
  switch (campaign.status) {
    case "draft": {
      if (f.seriesKnown === false) return [EDIT, ...test];
      if (series !== null) return series.status === "blocked" ? [EDIT, ...seriesAct(series)] : [EDIT, ...test, ...seriesAct(series)];
      if (blocker(f) !== "") return [EDIT, ...test];
      return [EDIT, ...test, savedTime(f) === "future" ? SCHEDULE : SEND];
    }
    case "scheduled":
      return [CANCEL, ...test, ...(blocker(f) === "" ? [SEND] : [])];
    case "sending":
      return [...test, PAUSE];
    case "paused":
      // Its template was pinned when the send began: only sending decides a resume.
      return [CANCEL, ...test, ...(campaign.accountId !== "" && sendingBlocker(f) === "" ? [RESUME] : [])];
    case "sent":
    case "cancelled":
    case "failed":
      // A series made from it before it was sent still runs, and this is its only page.
      return [...test, DUPLICATE, ...(f.seriesKnown === false ? [] : seriesAct(series))];
    default:
      // A status this build does not know: nothing is offered over a send that may be in flight.
      return [];
  }
}

/** A repeating draft reads as its series does. */
function seriesReading(series: SeriesRow, zone: string): CampaignReading {
  const tz = series.timeZone || zone;
  switch (series.status) {
    case "active":
      return {
        word: WORDS.repeating.active!,
        detail: series.nextAt === "" ? WORDS.every(series.intervalWeeks) : `${WORDS.every(series.intervalWeeks)} · next ${momentWords(series.nextAt, tz)} (${tz})`,
        tone: "live",
      };
    case "paused":
      return { word: WORDS.repeating.paused!, detail: WORDS.every(series.intervalWeeks), tone: "paused" };
    case "blocked":
      return { word: WORDS.repeating.blocked!, detail: series.lastError, tone: "none" };
    default:
      return { word: WORDS.campaign.draft!, detail: "", tone: "none" };
  }
}

export function campaignReading(f: CampaignFacts): CampaignReading {
  const { campaign, series } = f;
  const zone = f.zone ?? browserZone();
  const word = WORDS.campaign[campaign.status] ?? "";
  switch (campaign.status) {
    case "draft": {
      if (f.seriesKnown === false) return { word, detail: "", tone: "none" };
      if (series !== null) return seriesReading(series, zone);
      const blocked = blocker(f);
      if (blocked === null) return { word, detail: "", tone: "none" };
      if (blocked !== "") return { word, detail: blocked, tone: "none" };
      return { word, detail: savedTime(f) === "past" ? "Its time has passed." : "", tone: "none" };
    }
    case "scheduled": {
      const blocked = blocker(f);
      return { word, detail: blocked || (campaign.scheduledAt === "" ? "" : momentWords(campaign.scheduledAt, zone)), tone: "none" };
    }
    case "sending":
      return { word, detail: progress(campaign), tone: "live" };
    case "paused":
      return { word, detail: campaign.accountId === "" ? "" : sendingBlocker(f) ?? "", tone: "paused" };
    case "sent":
      return { word, detail: campaign.completedAt === "" ? "" : momentWords(campaign.completedAt, zone), tone: "none" };
    case "failed":
      return { word, detail: campaign.lastError, tone: "none" };
    default:
      return { word, detail: "", tone: "none" };
  }
}

/** "1,204 of 1,250 sent", or nothing while the roster is not frozen yet. */
function progress(campaign: CampaignRow): string {
  if (campaign.recipientCount === 0) return "";
  return `${campaign.sentCount.toLocaleString("en-US")} of ${campaign.recipientCount.toLocaleString("en-US")} sent`;
}

/**
 * The question an act asks in the bar before it does anything that cannot be
 * taken back. A send names how many people it reaches (an unread count is left
 * out, never guessed); a schedule names when; a cancel says what stays.
 */
export function confirmSentence(act: "send" | "schedule" | "cancel" | "cancel-draft", f: CampaignFacts, subscribers: Figure): string {
  const name = `“${campaignName(f.campaign)}”`;
  const count = subscribers.kind === "measured" ? WORDS.subscribers(subscribers.value) : "";
  const zone = f.zone ?? browserZone();
  switch (act) {
    case "send":
      return count === "" ? `Send ${name} now?` : `Send ${name} to ${count} now?`;
    case "schedule": {
      const at = `${momentWords(f.campaign.scheduledAt, zone)} (${zone})`;
      return count === "" ? `Schedule ${name} for ${at}?` : `Schedule ${name} for ${at}, to ${count}?`;
    }
    case "cancel": {
      const sent = f.campaign.sentCount;
      if (sent === 1) return `Cancel ${name}? The one already sent stays sent, and nobody else will get it.`;
      if (sent > 1) return `Cancel ${name}? The ${sent.toLocaleString("en-US")} already sent stay sent, and nobody else will get it.`;
      return `Cancel ${name}? Nobody will get it, and it stays on the list as cancelled.`;
    }
    case "cancel-draft": {
      // An occurrence is made from what the series captured, not from this draft.
      const repeats = f.series?.status === "active" ? " Its repeating sends go on until you pause them." : "";
      return `Cancel ${name}? It won't be sent, and it stays on the list as cancelled.${repeats}`;
    }
  }
}

/** The question Start repeating asks: who, how often, from when. The audience's size moves between sends, so it is named, not counted. */
export function repeatSentence(campaign: string, audience: string, when: Extract<When, { kind: "repeat" }>, zone: string): string {
  return `Send “${campaign}” to ${audience} ${whenWords(when, zone)}?`;
}
