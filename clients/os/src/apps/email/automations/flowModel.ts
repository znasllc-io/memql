import { newShortId } from "@znasllc-io/memql-sdk-core/client";
export type StepKind = "send" | "confirm" | "wait";
export interface FlowStep { id: string; kind: StepKind; templateId?: string; senderIdentityId?: string; days?: number; hours?: number; minutes?: number; timeoutDays?: number }
/** A wait's whole length in minutes. */
export function waitMinutes(s: FlowStep): number { return (s.days ?? 0) * 1440 + (s.hours ?? 0) * 60 + (s.minutes ?? 0); }
/** A minutes total as the days, hours (0-23) and minutes (0-59) the engine stores. */
export function splitMinutes(total: number): Pick<FlowStep, "days" | "hours" | "minutes"> { return { days: Math.floor(total / 1440), hours: Math.floor((total % 1440) / 60), minutes: total % 60 }; }
export interface Trigger { kind: "joined" | "confirmed"; audienceId: string }
export const STEP_ID = /^[A-Za-z0-9_-]{1,40}$/;
export function newStep(kind: StepKind, defaults: { senderIdentityId?: string } = {}): FlowStep { return kind === "wait" ? { id: newShortId(), kind, days: 1, hours: 0 } : { id: newShortId(), kind, ...defaults, ...(kind === "confirm" ? { timeoutDays: 7 } : {}) }; }
export function insertStep(steps: readonly FlowStep[], index: number, step: FlowStep): FlowStep[] { const next = [...steps]; next.splice(index, 0, step); return next; }
export function moveStep(steps: readonly FlowStep[], index: number, delta: -1 | 1): FlowStep[] { if (index < 0 || index + delta < 0 || index + delta >= steps.length) return [...steps]; const next = [...steps]; [next[index], next[index + delta]] = [next[index + delta]!, next[index]!]; return next; }
export function removeStep(steps: readonly FlowStep[], index: number): FlowStep[] { return steps.filter((_, i) => i !== index); }
export function updateStep(steps: readonly FlowStep[], id: string, patch: Partial<FlowStep>): FlowStep[] { return steps.map(s => s.id === id ? { ...s, ...patch, id } : s); }
export function wireSteps(steps: readonly FlowStep[]): Record<string, unknown>[] { return steps.map(s => s.kind === "wait" ? { id: s.id, kind: s.kind, days: s.days ?? 0, hours: s.hours ?? 0, ...(s.minutes ? { minutes: s.minutes } : {}) } : { id: s.id, kind: s.kind, ...(s.templateId ? { templateId: s.templateId } : {}), ...(s.senderIdentityId ? { senderIdentityId: s.senderIdentityId } : {}), ...(s.kind === "confirm" ? { timeoutDays: s.timeoutDays ?? 7 } : {}) }); }
export function triggerSentence(t: Trigger, audienceName: string): string { return t.kind === "joined" ? `When someone joins ${audienceName}` : `When someone confirms their subscription to ${audienceName}`; }
export function stepSentence(s: FlowStep, names: { template: string }): string {
  if (s.kind === "wait") { const parts = [s.days ? `${s.days} ${s.days === 1 ? "day" : "days"}` : "", s.hours ? `${s.hours} ${s.hours === 1 ? "hour" : "hours"}` : "", s.minutes ? `${s.minutes} ${s.minutes === 1 ? "minute" : "minutes"}` : ""].filter(Boolean); return `Wait ${parts.length > 2 ? `${parts.slice(0, -1).join(", ")} and ${parts.at(-1)}` : parts.join(" and ") || "0 minutes"}`; }
  if (!names.template) return s.kind === "confirm" ? "Ask to confirm: choose a template" : "Send an email: choose a template";
  return s.kind === "confirm" ? `Send ${names.template} and wait for confirmation, up to ${s.timeoutDays ?? 7} days` : `Send ${names.template}`;
}
export interface CardCount { cardId: string; label: string; args: { journeyId: string; status?: string; stepId?: string } }
/**
 * The statuses an automation run can be in, in the order people read them, with
 * the words for each: the People section's, and, lower-cased, the Flow's counts.
 * `waiting` is a run waiting for the person to confirm: the engine writes it only
 * at a confirm step, once the confirmation email went out. A wait step's own
 * count is something else, the running runs sitting in that wait.
 */
export const RUN_STATUSES = [["active", "In a run"], ["waiting", "Waiting to confirm"], ["completed", "Finished"], ["exited", "Left early"], ["failed", "Failed"]] as const;
const STATE_WORDS: Readonly<Record<string, string>> = Object.fromEntries(RUN_STATUSES.map(([status, words]) => [status, words.toLowerCase()]));
export function countQueries(journeyId: string, steps: readonly FlowStep[]): CardCount[] {
  return [{ cardId: "trigger", label: "enrolled", args: { journeyId } }, ...steps.flatMap((step, i): CardCount[] => step.kind === "confirm" ? [{ cardId: step.id, label: STATE_WORDS.waiting!, args: { journeyId, status: "waiting", stepId: step.id } }] : step.kind === "wait" ? [{ cardId: step.id, label: "waiting", args: { journeyId, status: "active", stepId: steps[i + 1]?.id ?? "" } }] : []), ...["completed", "exited", "failed"].map(status => ({ cardId: "end", label: STATE_WORDS[status]!, args: { journeyId, status } }))];
}
