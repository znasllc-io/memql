import { isPublished } from "../rows";
import type { AudienceRow, TemplateRow, SenderIdentityRow } from "../rows";
import { isOperatorOrganization } from "../organizations";
import { STEP_ID, type FlowStep, type Trigger } from "./flowModel";
export interface Issue { stepId: string; text: string }
export function journeyIssues(draft: { accountId: string; trigger: Trigger | null; steps: readonly FlowStep[] }, feeds: { audiences: AudienceRow[]; templates: TemplateRow[]; senders: SenderIdentityRow[]; confirms?: ReadonlyMap<string, boolean> }): Issue[] {
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
    if (step.kind === "wait") { const days = step.days ?? 0, hours = step.hours ?? 0; if (!Number.isInteger(days) || !Number.isInteger(hours) || days < 0 || days > 365 || hours < 0 || hours > 23 || days * 24 + hours < 1 || days * 24 + hours > 365 * 24) add(step.id, "A wait lasts from one hour to 365 days."); continue; }
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
  }
  return issues;
}
