import { isTransactional, type RecipientMode, type TemplateRow } from "../rows";
export type TriggerChoice = "joined" | "confirmed" | "event";
export type AddStep = "organization" | "trigger" | "audience" | "steps" | "what" | "event" | "condition" | "who" | "template" | "sender" | "review";
export function addSteps(choice: TriggerChoice | null, who: RecipientMode | null, chooseOrganization: boolean): AddStep[] {
  return [...(chooseOrganization ? ["organization" as const] : []), "trigger", ...(choice === "event" ? ["what", "event", "condition", "who", "template", ...(who === "cluster_roles" ? [] : ["sender"]), "review"] as AddStep[] : choice ? ["audience", "steps", "review"] as AddStep[] : [])];
}
/**
 * The templates an automation's step may send, a journey's and an event rule's
 * alike: the organization's own that are not put away, and none that is
 * transactional -- both mail more than the one person such a template is about.
 */
export function stepTemplates(all: readonly TemplateRow[], accountId: string): TemplateRow[] {
  return all.filter(t => t.accountId === accountId && t.status !== "archived" && !isTransactional(t));
}
