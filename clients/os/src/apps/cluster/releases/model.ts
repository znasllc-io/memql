import type { Result } from "@znasllc-io/memql-sdk-core/client";

export type CandidateState = "preparing" | "ready" | "approved" | "retired";
export interface ComponentSummary { name: string; version: string; repository: string; commit: string }
export interface Artifact { name: string; kind: "oci" | "file"; platform: string; digest: string; imageDigest?: string; size: number }
export interface Destination { targetId: string; targetDigest: string; component: string; artifact: string; operation: "publish" | "install" }
export interface CandidateSummary { candidateId: string; state: CandidateState; components: ComponentSummary[]; createdAt: string; evidenceCount: number }
export interface Candidate {
  candidateId: string; state: CandidateState; approvalId?: string;
  manifest: {
    components: (ComponentSummary & { artifacts: Artifact[] })[];
    evidence: { name: string; component: string; workRunId: string; stepKey: string; attempt: number; receiptDigest: string }[];
    compatibility: { component: string; requires: string; minVersion: string; maxExclusive: string }[];
    destinations: Destination[];
  };
}
export interface Publication { candidateId: string; approvalId: string; effectId: string; state: "pending" | "complete"; target: Destination; artifactKind: "oci" | "file" }
export interface Target extends Destination { origin: string; repository: string; kind: "oci" | "file"; tag?: string; assetName?: string; releaseId?: number }
export interface CandidatePage { candidates: CandidateSummary[]; nextCursor?: string }

function object(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("The cluster returned an invalid release record.");
  return value as Record<string, unknown>;
}
function string(value: unknown): string {
  if (typeof value !== "string" || !value) throw new Error("The cluster returned an incomplete release record.");
  return value;
}
function array(value: unknown, limit = 1024): unknown[] {
  if (!Array.isArray(value) || value.length > limit) throw new Error("The cluster returned an invalid release collection.");
  return value;
}
function digest(value: unknown): string {
  const result = string(value);
  if (!/^sha256:[a-f0-9]{64}$/.test(result)) throw new Error("The cluster returned an invalid release identity.");
  return result;
}
function state(value: unknown): CandidateState {
  if (!["preparing", "ready", "approved", "retired"].includes(string(value))) throw new Error("This release state is not supported.");
  return value as CandidateState;
}
function component(value: unknown): ComponentSummary {
  const v = object(value);
  return { name: string(v.name), version: string(v.version), repository: string(v.repository), commit: string(v.commit) };
}
function destination(value: unknown): Destination {
  const v = object(value);
  if (v.operation !== "publish" && v.operation !== "install") throw new Error("This release destination is not supported.");
  return { targetId: string(v.targetId), targetDigest: digest(v.targetDigest), component: string(v.component), artifact: string(v.artifact), operation: v.operation };
}
export function body(result: Result): Record<string, unknown> { return object(result.single()); }
export function readPage(result: Result): CandidatePage {
  const v = body(result);
  return { candidates: array(v.candidates, 50).map((entry) => {
    const r = object(entry);
    if (typeof r.evidenceCount !== "number" || !Number.isSafeInteger(r.evidenceCount) || r.evidenceCount < 0) throw new Error("The cluster returned an invalid evidence count.");
    return { candidateId: digest(r.candidateId), state: state(r.state), createdAt: string(r.createdAt), evidenceCount: r.evidenceCount, components: array(r.components, 64).map(component) };
  }), nextCursor: v.nextCursor === undefined ? undefined : string(v.nextCursor) };
}
export function readCandidate(result: Result, expected: string): Candidate {
  const v = body(result), m = object(v.manifest);
  if (digest(v.candidateId) !== expected) throw new Error("The cluster returned a different release.");
  const s = state(v.state);
  return { candidateId: expected, state: s, approvalId: s === "approved" ? string(v.approvalId) : undefined, manifest: {
    components: array(m.components, 64).map((entry) => ({ ...component(entry), artifacts: array(object(entry).artifacts, 128).map((entry) => {
      const a = object(entry);
      if ((a.kind !== "oci" && a.kind !== "file") || typeof a.size !== "number" || !Number.isSafeInteger(a.size) || a.size < 0) throw new Error("The cluster returned an invalid release artifact.");
      return { name: string(a.name), kind: a.kind, platform: string(a.platform), digest: digest(a.digest), imageDigest: a.kind === "oci" ? digest(a.imageDigest) : undefined, size: a.size };
    }) })),
    destinations: array(m.destinations).map(destination),
    evidence: array(m.evidence).map((entry) => {
      const e = object(entry);
      if (typeof e.attempt !== "number" || !Number.isSafeInteger(e.attempt) || e.attempt < 1) throw new Error("The cluster returned an invalid verification attempt.");
      return { name: string(e.name), component: string(e.component), workRunId: string(e.workRunId), stepKey: string(e.stepKey), attempt: e.attempt, receiptDigest: digest(e.receiptDigest) };
    }),
    compatibility: array(m.compatibility ?? [], 256).map((entry) => {
      const r = object(entry);
      return { component: string(r.component), requires: string(r.requires), minVersion: string(r.minVersion), maxExclusive: string(r.maxExclusive) };
    }),
  } };
}
export function readPublications(result: Result, expected: string): Publication[] {
  const v = body(result);
  if (digest(v.candidateId) !== expected) throw new Error("The cluster returned publication history for another release.");
  return array(v.publications).map((entry) => {
    const p = object(entry), kind = object(p.artifact).kind;
    if (kind !== "oci" && kind !== "file") throw new Error("The publication artifact is not supported.");
    if (digest(p.candidateId) !== expected || (p.state !== "pending" && p.state !== "complete")) throw new Error("The cluster returned inconsistent publication history.");
    return { candidateId: expected, approvalId: string(p.approvalId), effectId: string(p.effectId), state: p.state, target: destination(p.target), artifactKind: kind };
  });
}
export function readTargets(result: Result): Target[] {
  return array(body(result).targets).map((entry) => {
    const t = object(entry);
    if (t.kind === "file" && (typeof t.releaseId !== "number" || !Number.isSafeInteger(t.releaseId) || t.releaseId < 1)) throw new Error("The draft release identity is unavailable.");
    if (t.kind !== "oci" && t.kind !== "file") throw new Error("This publication destination is not supported.");
    return { ...destination(t), kind: t.kind, origin: string(t.origin), repository: string(t.repository), tag: typeof t.tag === "string" ? t.tag : undefined, assetName: typeof t.assetName === "string" ? t.assetName : undefined, releaseId: typeof t.releaseId === "number" ? t.releaseId : undefined };
  });
}
export function destinationKey(d: Destination): string { return `${d.targetId}/${d.component}/${d.artifact}/${d.operation}`; }
export function matchingTarget(d: Destination, targets: Target[] | null): Target | undefined {
  return targets?.find((t) => destinationKey(t) === destinationKey(d) && t.targetDigest === d.targetDigest);
}
export function candidateTitle(c: Pick<CandidateSummary, "components">): string { return c.components.map((x) => `${x.name} ${x.version}`).join(", "); }
export const stateLabel: Record<CandidateState, string> = { preparing: "Verification incomplete", ready: "Ready for review", approved: "Approved", retired: "Retired" };
