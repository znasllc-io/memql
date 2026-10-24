import type { QueryClient, Result } from "@znasllc-io/memql-sdk-core/client";
import { body } from "./model";

export type BuildPhase = "prepared" | "tagging" | "building" | "built" | "ready" | "publishing" | "published" | "blocked" | "uncertain";
export type BuildAction = "start" | "observe" | "resume" | "approve" | "publish" | "view_candidate";
export interface ReleaseBuild {
  intentId: string; profileName: string; configuration: string; phase: BuildPhase;
  repository: string; component: string; version: string; tag: string; sha: string; pipelineId: string;
  pipelineRunId: string; workRunId: string; candidateId: string; approvalId: string;
  reasonCode: string; reason: string; actions: BuildAction[]; createdAt: string; updatedAt: string;
  catalog: { state: "absent" | "sealed" | "unavailable"; digest?: string };
}
type BuildMethods = Pick<QueryClient, "releasePrepareBuild" | "releaseBuilds" | "releaseGetBuild" | "releaseStartBuild" | "releaseObserveBuild" | "releaseResumeBuild" | "releasePublishBuild">;
export type ReleaseBuildQueries = BuildMethods & Pick<QueryClient, "pipelinesForOwner" | "pipelineForOwner" | "pipelineRunForOwner">;
type Arguments = { [Name in keyof BuildMethods]: Parameters<BuildMethods[Name]>[0] };
// Keep the action set closed while using the generated argument and wire contract.
export function buildCall<Name extends keyof Arguments>(query: BuildMethods, name: Name, args: Arguments[Name], signal?: AbortSignal) {
  const dispatch: { [Key in keyof Arguments]: (input: Arguments[Key]) => Promise<Result> } = {
    releasePrepareBuild: input => query.releasePrepareBuild(input, { signal }),
    releaseBuilds: input => query.releaseBuilds(input, { signal }),
    releaseGetBuild: input => query.releaseGetBuild(input, { signal }),
    releaseStartBuild: input => query.releaseStartBuild(input, { signal }),
    releaseObserveBuild: input => query.releaseObserveBuild(input, { signal }),
    releaseResumeBuild: input => query.releaseResumeBuild(input, { signal }),
    releasePublishBuild: input => query.releasePublishBuild(input, { signal }),
  };
  return dispatch[name](args);
}
const phases: BuildPhase[] = ["prepared", "tagging", "building", "built", "ready", "publishing", "published", "blocked", "uncertain"];
const actions: BuildAction[] = ["start", "observe", "resume", "approve", "publish", "view_candidate"];
function object(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("The release progress could not be read.");
  return value as Record<string, unknown>;
}
function text(value: unknown, required = false): string {
  if (value === undefined && !required) return "";
  if (typeof value !== "string" || (required && !value)) throw new Error("The release progress is incomplete.");
  return value;
}
function digest(value: unknown): string {
  const v = text(value, true);
  if (!/^sha256:[a-f0-9]{64}$/.test(v)) throw new Error("The release reference is invalid.");
  return v;
}
function build(value: unknown, expected?: string): ReleaseBuild {
  const v = object(value), intentId = text(v.intentId, true);
  if (expected && intentId !== expected) throw new Error("The cluster returned a different release request.");
  if (!phases.includes(v.phase as BuildPhase) || !Array.isArray(v.actions) || v.actions.length > actions.length || v.actions.some(a => !actions.includes(a)) || new Set(v.actions).size !== v.actions.length) throw new Error("The release progress has an unsupported state.");
  const sha = text(v.sha);
  if (sha && !/^[a-f0-9]{40}$/.test(sha)) throw new Error("The release source revision is invalid.");
  const candidateId = v.candidateId ? digest(v.candidateId) : "", approvalId = text(v.approvalId);
  const c = v.catalog === undefined ? { state: "unavailable" } : object(v.catalog);
  if (!["absent", "sealed", "unavailable"].includes(String(c.state))) throw new Error("The release catalog state is unavailable.");
  const catalog: ReleaseBuild["catalog"] = { state: c.state as ReleaseBuild["catalog"]["state"] };
  if (c.state === "sealed") {
    if (!candidateId || !approvalId) throw new Error("The signed release has no recorded candidate approval.");
    catalog.digest = digest(c.digest);
  }
  return { intentId, profileName: text(v.profileName, true), configuration: digest(v.configuration), phase: v.phase as BuildPhase,
    repository: text(v.repository, true), component: text(v.component, true), version: text(v.version), tag: text(v.tag), sha,
    pipelineId: text(v.pipelineId, true), pipelineRunId: text(v.pipelineRunId), workRunId: text(v.workRunId), candidateId, approvalId,
    reasonCode: text(v.reasonCode), reason: text(v.reason), actions: v.actions as BuildAction[], createdAt: text(v.createdAt, true), updatedAt: text(v.updatedAt, true), catalog };
}
export function readReleaseBuild(result: Result, expected?: string) { return build(body(result), expected); }
export function readReleaseBuilds(result: Result): ReleaseBuild[] {
  const v = body(result);
  if (!Array.isArray(v.intents) || v.intents.length > 100) throw new Error("Release requests could not be read.");
  const rows = v.intents.map(item => build(item));
  if (new Set(rows.map(r => r.intentId)).size !== rows.length) throw new Error("The release list contains conflicting requests.");
  return rows;
}
export function buildTitle(build: ReleaseBuild) { return [build.component, build.version].filter(Boolean).join(" "); }
export function buildWord(build: ReleaseBuild): string {
  if (build.catalog.state === "sealed" && build.phase === "published") return "In release catalog";
  const words: Record<BuildPhase, string> = { prepared: "Ready to build", tagging: "Checking release tag", building: "Build in progress", built: "Build complete", ready: build.approvalId ? "Approved for publication" : "Ready for review", publishing: "Publication in progress", published: "Checking release catalog", blocked: "Release needs attention", uncertain: "Outcome not yet verified" };
  return words[build.phase];
}
export type JourneyStep = "release" | "source" | "version" | "build" | "review" | "publish" | "catalog";
export function currentStep(build: ReleaseBuild): JourneyStep {
  if (build.phase === "published") return "catalog";
  if (build.phase === "publishing" || build.approvalId) return "publish";
  if (build.candidateId) return "review";
  if (build.phase === "building" || build.phase === "built" || build.pipelineRunId || build.phase === "tagging" || build.phase === "uncertain") return "build";
  return "version";
}
