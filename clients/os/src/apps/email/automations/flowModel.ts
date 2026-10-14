import { newShortId } from "@znasllc-io/memql-sdk-core/client";
export type StepKind = "send" | "confirm" | "wait";
export interface FlowStep { id: string; kind: StepKind; templateId?: string; senderIdentityId?: string; days?: number; hours?: number; timeoutDays?: number }
export interface Trigger { kind: "joined" | "confirmed"; audienceId: string }
export const STEP_ID = /^[A-Za-z0-9_-]{1,40}$/;
export function newStep(kind: StepKind, defaults: { senderIdentityId?: string } = {}): FlowStep { return kind === "wait" ? { id: newShortId(), kind, days: 1, hours: 0 } : { id: newShortId(), kind, ...defaults, ...(kind === "confirm" ? { timeoutDays: 7 } : {}) }; }
export function insertStep(steps: readonly FlowStep[], index: number, step: FlowStep): FlowStep[] { const next = [...steps]; next.splice(index, 0, step); return next; }
export function moveStep(steps: readonly FlowStep[], index: number, delta: -1 | 1): FlowStep[] { if (index < 0 || index + delta < 0 || index + delta >= steps.length) return [...steps]; const next = [...steps]; [next[index], next[index + delta]] = [next[index + delta]!, next[index]!]; return next; }
export function removeStep(steps: readonly FlowStep[], index: number): FlowStep[] { return steps.filter((_, i) => i !== index); }
export function updateStep(steps: readonly FlowStep[], id: string, patch: Partial<FlowStep>): FlowStep[] { return steps.map(s => s.id === id ? { ...s, ...patch, id } : s); }
export function wireSteps(steps: readonly FlowStep[]): Record<string, unknown>[] { return steps.map(s => s.kind === "wait" ? { id: s.id, kind: s.kind, days: s.days ?? 0, hours: s.hours ?? 0 } : { id: s.id, kind: s.kind, ...(s.templateId ? { templateId: s.templateId } : {}), ...(s.senderIdentityId ? { senderIdentityId: s.senderIdentityId } : {}), ...(s.kind === "confirm" ? { timeoutDays: s.timeoutDays ?? 7 } : {}) }); }
export function triggerSentence(t: Trigger, audienceName: string): string { return t.kind === "joined" ? `When someone joins ${audienceName}` : `When someone confirms their subscription to ${audienceName}`; }
export function stepSentence(s: FlowStep, names: { template: string }): string {
  if (s.kind === "wait") { const parts = [s.days ? `${s.days} ${s.days === 1 ? "day" : "days"}` : "", s.hours ? `${s.hours} ${s.hours === 1 ? "hour" : "hours"}` : ""].filter(Boolean); return `Wait ${parts.join(" and ") || "0 hours"}`; }
  if (!names.template) return s.kind === "confirm" ? "Ask to confirm: choose a template" : "Send an email: choose a template";
  return s.kind === "confirm" ? `Send ${names.template} and wait for confirmation, up to ${s.timeoutDays ?? 7} days` : `Send ${names.template}`;
}
export interface CardCount { cardId: string; label: string; args: { journeyId: string; status?: string; stepId?: string } }
export function countQueries(journeyId: string, steps: readonly FlowStep[]): CardCount[] {
  return [{ cardId: "trigger", label: "enrolled", args: { journeyId } }, ...steps.flatMap((step, i): CardCount[] => step.kind === "confirm" ? [{ cardId: step.id, label: "waiting", args: { journeyId, status: "waiting", stepId: step.id } }] : step.kind === "wait" ? [{ cardId: step.id, label: "waiting", args: { journeyId, status: "active", stepId: steps[i + 1]?.id ?? "" } }] : []), ...["completed", "exited", "failed"].map(status => ({ cardId: "end", label: status === "exited" ? "left early" : status, args: { journeyId, status } }))];
}
