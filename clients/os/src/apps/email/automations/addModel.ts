import type { RecipientMode } from "../rows";
export type TriggerChoice = "joined" | "confirmed" | "event";
export type AddStep = "organization" | "trigger" | "audience" | "steps" | "what" | "event" | "condition" | "who" | "template" | "sender" | "review";
export function addSteps(choice: TriggerChoice | null, who: RecipientMode | null, chooseOrganization: boolean): AddStep[] {
  return [...(chooseOrganization ? ["organization" as const] : []), "trigger", ...(choice === "event" ? ["what", "event", "condition", "who", "template", ...(who === "cluster_roles" ? [] : ["sender"]), "review"] as AddStep[] : choice ? ["audience", "steps", "review"] as AddStep[] : [])];
}
