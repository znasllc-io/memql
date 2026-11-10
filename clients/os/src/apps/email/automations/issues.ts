import { isPublished } from "../rows";
import type { AudienceRow, TemplateRow, SenderIdentityRow } from "../rows";
import { isOperatorOrganization } from "../organizations";
import type { SenderRefusal } from "../senderChecks";
import { STEP_ID, type FlowStep, type Trigger } from "./flowModel";
/**
 * Something that stands between an automation and turning it on, at a step (""
 * for the automation as a whole). `whileOn` is what it means for an automation
 * already on, when that is not simply that its emails are held: the engine
 * holds a missing or turned-off sender's emails, but a send as a sender the
 * provider refuses fails.
 */
export interface Issue { stepId: string; text: string; whileOn?: string }

/**
 * The issue with sending as a sender the provider would refuse, for a reason a
 * person fixes on the sender or its sending domain; null when there is none.
 * `retries`: whether this automation's failed sends can be tried again, as a
 * series' runs can (Retry, on its page) and an event automation's sends cannot.
 */
export function senderRefusalIssue(stepId: string, address: string, refusal: SenderRefusal, retries: boolean): Issue | null {
  const then = retries ? " Failed runs can then be retried." : "";
  switch (refusal) {
    case "unregistered":
      return { stepId, text: `The sender ${address} is not registered with Azure. Add it on the sending domain before turning this on.`, whileOn: `Sends from ${address} fail until it is added on the sending domain.${then}` };
    case "renamed":
      return { stepId, text: `Azure sends ${address} under a different name. Give the sender the display name Azure has before turning this on.`, whileOn: `Sends from ${address} fail until its display name is the one Azure has.${then}` };
    case "azureOwnDomain":
      return { stepId, text: `This organization sends through Azure's own domain, which sends only as its own sender, not ${address}. Choose that sender before turning this on.`, whileOn: `Sends from ${address} fail: Azure's own domain sends only as its own sender. ${retries ? "Pause, choose that sender, then retry the failed runs." : "Pause and choose that sender."}` };
    default:
      return null;
  }
}

/**
 * What stands between an automation and turning it on, by step. `checks` is the
 * engine's sending check for each sender, by id (`useSenderChecks`): a sender
 * the provider would refuse is an issue, for anyone who may turn it on.
 */
export function journeyIssues(draft: { accountId: string; trigger: Trigger | null; steps: readonly FlowStep[] }, feeds: { audiences: AudienceRow[]; templates: TemplateRow[]; senders: SenderIdentityRow[]; confirms?: ReadonlyMap<string, boolean>; checks?: ReadonlyMap<string, SenderRefusal> }): Issue[] {
  const issues: Issue[] = [];
  const add = (stepId: string, text: string) => issues.push({ stepId, text });
  const audience = feeds.audiences.find(a => a.id === draft.trigger?.audienceId);
  if (!draft.trigger?.audienceId) add("", "Choose an audience.");
  else if (!audience || audience.accountId !== draft.accountId) add("", "Choose an audience in this organization.");
  else if (audience.status === "archived") add("", `The audience “${audience.name}” is archived.`);
  if (!draft.steps.length || draft.steps.length > 20) add("", "An automation has 1 to 20 steps.");
  if (!draft.steps.some(s => s.kind !== "wait")) add("", "Add at least one email.");
  if (draft.trigger?.kind === "joined" && audience?.confirmation === "required" && draft.steps.find(s => s.kind !== "wait")?.kind !== "confirm") add("", "This audience asks people to confirm, so the first email must ask them to confirm.");
  const ids = new Set<string>(); let confirms = 0;
  for (const step of draft.steps) {
    if (!STEP_ID.test(step.id) || ids.has(step.id)) add(step.id, "Give each step a unique id of up to 40 letters, digits, hyphens or underscores."); ids.add(step.id);
    if (step.kind === "wait") { const days = step.days ?? 0, hours = step.hours ?? 0, minutes = step.minutes ?? 0, total = days * 1440 + hours * 60 + minutes; if (![days, hours, minutes].every(Number.isInteger) || days < 0 || days > 365 || hours < 0 || hours > 23 || minutes < 0 || minutes > 59 || total < 1 || total > 365 * 1440) add(step.id, "A wait lasts from one minute to 365 days."); continue; }
    if (step.kind === "confirm") {
      confirms++; if (confirms > 1) add(step.id, "An automation asks people to confirm once.");
      if (draft.trigger?.kind === "confirmed") add(step.id, "People in this automation have already confirmed. Remove this step.");
      if (!Number.isInteger(step.timeoutDays ?? 7) || (step.timeoutDays ?? 7) < 1 || (step.timeoutDays ?? 7) > 30) add(step.id, "A confirmation waits 1 to 30 days.");
    }
    const template = feeds.templates.find(t => t.id === step.templateId);
    if (!step.templateId) add(step.id, "Choose a template.");
    else if (!template || template.accountId !== draft.accountId) add(step.id, "Choose a template in this organization.");
    else {
      if (!isPublished(template)) add(step.id, `Publish “${template.name}” before turning this on.`);
      if (step.kind === "confirm" && feeds.confirms?.get(template.id) === false) add(step.id, `Add {{confirmUrl}} to “${template.name}” so people can confirm.`);
    }
    const sender = feeds.senders.find(s => s.id === step.senderIdentityId);
    if (!step.senderIdentityId) { if (!isOperatorOrganization(draft.accountId)) add(step.id, "Choose a sender."); }
    else if (!sender || sender.accountId !== draft.accountId) add(step.id, "Choose a sender in this organization.");
    else if (sender.status === "disabled") add(step.id, `The sender ${sender.address} is turned off.`);
    else {
      const refused = senderRefusalIssue(step.id, sender.address, feeds.checks?.get(sender.id) ?? "", true);
      if (refused) issues.push(refused);
    }
  }
  return issues;
}
