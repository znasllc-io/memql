import type { Figure } from "../../../kit/measure";
import type { ActSpec } from "../acts";
import { templateName, type TemplateRow } from "../rows";
import { localToInstant, momentWords, whenWords } from "../when";
import { WORDS } from "../words";

// ADD A CAMPAIGN, AS A PURE MODEL (memql#6033, Ruling 8).
//
// The wizard asks one question a step: whose (only when there is a choice), who
// gets it, what it says, who it comes from, when it goes -- then Review. Which of
// those it walks, which it has reached, what its floor says on Review and which
// acts it offers there are functions of the answers and the facts, kept here so
// "an act that is not legal is ABSENT, never disabled" is a property of a list
// that does not hold it, asserted without a DOM.
//
// REVIEW NAMES THE FIRST THING MISSING, in the order a person would fix them: a
// template still to publish, sending not set up for the organization, an
// audience nobody is subscribed to, a time already gone. Until nothing is, the
// forward act is absent and the words say what to do. Saving a draft needs none
// of it, so Save as draft is always there before a draft exists.

export type CampaignStep = "organization" | "audience" | "template" | "sender" | "when" | "review";

/** When it goes. `at` is the browser's own `datetime-local` value: its zone, no offset. */
export type When = { kind: "now" } | { kind: "schedule"; at: string } | { kind: "repeat"; weeks: number; at: string };

export interface CampaignDraft {
  accountId: string;
  audienceId: string;
  templateId: string;
  /** Null until a sender is chosen; "" is this cluster's own sender, which only the operator organization has. */
  senderIdentityId: string | null;
  when: When | null;
  /** Blank means the template's name. */
  name: string;
  trackOpens: boolean;
  trackClicks: boolean;
}

export interface ReviewFacts {
  /** How many people the audience reaches; absent is not zero. */
  subscribers: Figure;
  /** The chosen template's row. */
  template?: TemplateRow;
  sendingReady: boolean;
  /** False while sending readiness is still being read: nothing is said about it then, and nothing is offered. */
  sendingKnown: boolean;
  /** The engine's reason sending is not ready, verbatim. */
  sendingReason: string;
  organizationName: string;
  /** The chosen audience's name, for the sentence that says it reaches nobody. */
  audienceName: string;
  /** A draft has been written under the wizard's id (a test, or a send that was refused). */
  draftExists: boolean;
  /** A test has somewhere to go and can go there now. */
  canTest: boolean;
}

export type Forward = "Send now" | "Schedule" | "Start repeating";

const ORDER: readonly CampaignStep[] = ["organization", "audience", "template", "sender", "when", "review"];

/** The steps a draft walks: the organization only when there is a choice to make, or nothing has answered it. */
export function stepsFor(draft: CampaignDraft, chooseOrganization: boolean): CampaignStep[] {
  return chooseOrganization || draft.accountId === "" ? [...ORDER] : ORDER.slice(1);
}

/** Whether When has a whole answer: now, or a time that is a time. */
function whenAnswered(when: When | null): boolean {
  return when !== null && (when.kind === "now" || localToInstant(when.at) !== null);
}

/** Whether a step has its answer. Review is never answered: it is where the acting happens. */
export function stepAnswered(step: CampaignStep, draft: CampaignDraft): boolean {
  switch (step) {
    case "organization":
      return draft.accountId !== "";
    case "audience":
      return draft.audienceId !== "";
    case "template":
      return draft.templateId !== "";
    case "sender":
      return draft.senderIdentityId !== null;
    case "when":
      return whenAnswered(draft.when);
    case "review":
      return false;
  }
}

/** A step the draft walks whose every earlier step is answered. */
export function stepReachable(step: CampaignStep, draft: CampaignDraft, chooseOrganization: boolean): boolean {
  const steps = stepsFor(draft, chooseOrganization);
  const at = steps.indexOf(step);
  return at >= 0 && steps.slice(0, at).every((earlier) => stepAnswered(earlier, draft));
}

/** Where a person goes after answering `step`: the next step still to answer, or Review. */
export function stepAfter(step: CampaignStep, draft: CampaignDraft, chooseOrganization: boolean): CampaignStep {
  const steps = stepsFor(draft, chooseOrganization);
  const later = steps.slice(steps.indexOf(step) + 1);
  return later.find((candidate) => !stepAnswered(candidate, draft)) ?? "review";
}

/** The first step still to answer: where the wizard opens. */
export function firstOpen(draft: CampaignDraft, chooseOrganization: boolean): CampaignStep {
  return stepsFor(draft, chooseOrganization).find((step) => !stepAnswered(step, draft)) ?? "review";
}

const FORWARD: Record<When["kind"], Forward> = { now: "Send now", schedule: "Schedule", repeat: "Start repeating" };
const READY: Record<When["kind"], string> = { now: "Ready to send", schedule: "Ready to schedule", repeat: "Ready to start repeating" };
const REPEAT_ON_A_DRAFT = "A draft doesn't repeat until you start it.";

/** Whether a When names a time that has passed (or is not a time). Now never has. */
function timeHasPassed(when: When, now: Date): boolean {
  if (when.kind === "now") return false;
  const instant = localToInstant(when.at);
  return instant === null || Date.parse(instant) <= now.getTime();
}

/**
 * What Review's floor says, and the act it offers when nothing is missing.
 *
 * `forward` is null while anything is: the act is absent, and `word` names the
 * first missing thing. A repeat cannot be stored on a draft, so whatever else
 * the floor says, a Repeat answer carries that as its detail -- it is what
 * Save as draft will not keep.
 */
export function reviewReading(draft: CampaignDraft, facts: ReviewFacts, now: Date): { word: string; detail: string; forward: Forward | null } {
  const kind = draft.when?.kind ?? "now";
  const note = kind === "repeat" ? REPEAT_ON_A_DRAFT : "";
  const missing = (word: string, detail = "") => ({ word, detail: detail || note, forward: null });

  const template = facts.template;
  if (template && template.status !== "ready") {
    return missing(template.status === "draft" ? `Publish “${templateName(template)}” before sending.` : "Choose a published template.");
  }
  // Not known yet is neither ready nor a problem: say nothing about it, offer nothing.
  if (!facts.sendingKnown) return { word: "Review the campaign", detail: "", forward: null };
  if (!facts.sendingReady) return missing(WORDS.sendingNotSetUp(facts.organizationName), facts.sendingReason);
  if (facts.subscribers.kind === "measured" && facts.subscribers.value === 0) return missing(`${facts.audienceName} has no subscribers.`);
  if (draft.when === null) return missing("Choose when it goes");
  if (timeHasPassed(draft.when, now)) return missing("Choose a time in the future.");
  return { word: READY[kind], detail: note, forward: FORWARD[kind] };
}

const FORWARD_ID: Record<Forward, string> = { "Send now": "send", Schedule: "schedule", "Start repeating": "repeat" };

/**
 * Review's floor (Ruling 8). Before anything is written: Send a test, Save as
 * draft, then the act -- the way out is the trail's Back. Once a draft exists
 * (a test made one, or a send was refused after making it): Cancel, which asks
 * and cancels that draft, Send a test, then the act. At most three, the primary
 * last, and an act that cannot run is not in the list.
 */
export function reviewActs(draft: CampaignDraft, facts: ReviewFacts, now: Date): ActSpec[] {
  const { forward } = reviewReading(draft, facts, now);
  const specs: ActSpec[] = [];
  if (facts.draftExists) specs.push({ id: "cancel", label: "Cancel", text: true });
  if (facts.canTest) specs.push({ id: "test", label: "Send a test", text: true });
  if (!facts.draftExists) specs.push({ id: "draft", label: "Save as draft", text: true });
  if (forward !== null) specs.push({ id: FORWARD_ID[forward], label: forward, tone: "primary" });
  return specs;
}

/**
 * The floor while it asks: Not yet beside the act it asked about, or Keep beside
 * Cancel campaign. A question about an act that stopped being legal asks
 * nothing.
 */
export function askingActs(asking: "forward" | "cancel", forward: Forward | null): ActSpec[] {
  if (asking === "cancel") {
    return [
      { id: "keep", label: "Keep", text: true },
      { id: "cancel-confirmed", label: "Cancel campaign", tone: "danger" },
    ];
  }
  if (forward === null) return [];
  return [
    { id: "not-yet", label: "Not yet", text: true },
    { id: "forward-confirmed", label: forward, tone: "primary" },
  ];
}

/**
 * The whole campaign as the sentence Review opens with: "Send “Welcome 2” to
 * Newsletter as news@acme.com on Tue, Oct 14 at 9:00 (America/Phoenix)."
 */
export function campaignSentence(names: { template: string; audience: string; sender: string }, when: When, zone: string): string {
  const at = when.kind === "now" ? "now" : `${whenWords(when, zone)} (${zone})`;
  return `Send “${names.template}” to ${names.audience} as ${names.sender} ${at}.`;
}

/** The name the campaign is created with: what was typed, else its template's. */
export function campaignNameFor(draft: CampaignDraft, template: TemplateRow | undefined): string {
  const typed = draft.name.trim();
  if (typed !== "") return typed;
  return template ? templateName(template) : "";
}

/** "1,204 subscribers", or nothing for a count that could not be read. */
function subscriberWords(subscribers: Figure): string {
  if (subscribers.kind !== "measured") return "";
  return `${subscribers.value.toLocaleString("en-US")} ${subscribers.value === 1 ? "subscriber" : "subscribers"}`;
}

/**
 * The question the forward act asks in the bar, naming how many people it
 * reaches: "Send “August update” to 1,204 subscribers now?". A count that could
 * not be read is left out rather than guessed at. A repeat names the audience,
 * whose size will move between sends.
 */
export function reviewQuestion(draft: CampaignDraft, facts: ReviewFacts, zone: string): string {
  const name = `“${campaignNameFor(draft, facts.template)}”`;
  const count = subscriberWords(facts.subscribers);
  const when = draft.when ?? { kind: "now" };
  switch (when.kind) {
    case "now":
      return count === "" ? `Send ${name} now?` : `Send ${name} to ${count} now?`;
    case "schedule": {
      const instant = localToInstant(when.at);
      const at = instant === null ? when.at : `${momentWords(instant, zone)} (${zone})`;
      return count === "" ? `Schedule ${name} for ${at}?` : `Schedule ${name} for ${at}, to ${count}?`;
    }
    case "repeat":
      return `Send ${name} to ${facts.audienceName} ${whenWords(when, zone)}?`;
  }
}
