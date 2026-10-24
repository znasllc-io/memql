import { Result } from "@znasllc-io/memql-sdk-core/client";
import type { BuildPhase, ReleaseBuild } from "../../src/apps/cluster/releases/journey";
import { releaseFixture, releaseId, releaseResult } from "./releaseFixtures";

export const intentId = `sha256:${"1".repeat(64)}`;
export const profileConfiguration = `sha256:${"2".repeat(64)}`;
export const releasePipeline = { id: "pipeline-source", packageId: "source-package", ownerUserId: "owner", name: "MemQL", repository: "example/memql", defaultBranch: "main", status: "active" };
export const releaseRun = { id: "release-run", pipelineId: releasePipeline.id, repository: releasePipeline.repository, sha: "d".repeat(40), version: "v0.25.0", event: "release", mode: "full", workRunId: "release-work", title: "Build engine 0.25.0", status: "in_progress", stages: [{ name: "Checks", status: "passed", steps: 4 }, { name: "Images", status: "running", steps: 7 }] };

/** Synthetic journal responses shared by component tests and the existing QA host. */
export function releaseJourneyFixture(phase: BuildPhase | "new" = "prepared") {
  const f = releaseFixture(phase === "publishing" || phase === "published" ? "approved" : "ready");
  f.state.profiles = [{ name: "engine", configuration: profileConfiguration, components: [{ name: "engine", repository: "example/memql", runs: ["release"] }], build: { component: "engine", run: "release", branch: "main" } }];
  f.state.empty = phase === "new" || !["ready", "publishing", "published"].includes(phase);
  const build: ReleaseBuild = {
    intentId, profileName: "engine", configuration: profileConfiguration, phase: phase === "new" ? "prepared" : phase,
    repository: "example/memql", component: "engine", version: "0.25.0", tag: "v0.25.0", sha: "d".repeat(40), pipelineId: releasePipeline.id,
    pipelineRunId: phase !== "new" && phase !== "prepared" && phase !== "blocked" ? releaseRun.id : "",
    workRunId: phase !== "new" && phase !== "prepared" && phase !== "blocked" ? releaseRun.workRunId : "",
    candidateId: ["ready", "publishing", "published"].includes(phase) ? releaseId : "",
    approvalId: ["publishing", "published"].includes(phase) ? "approval-exact" : "",
    reasonCode: "", reason: "", createdAt: "2026-10-06T18:00:00Z", updatedAt: "2026-10-06T18:01:00Z", catalog: { state: "absent" },
    actions: phase === "prepared" || phase === "new" ? ["start"] : phase === "building" ? ["observe"] : phase === "built" || phase === "uncertain" ? ["resume"] : phase === "ready" ? ["approve", "view_candidate"] : phase === "publishing" ? ["observe"] : [],
  };
  const state = { build, saved: phase !== "new", failedGet: false, lostPrepareReply: false, lostStartReply: false, lostPublishReply: false, syncApproval: true, pipelines: [releasePipeline], run: { ...releaseRun }, calls: [] as { name: string; call: string }[] };
  function seal() {
    state.build.phase = "published"; state.build.actions = ["view_candidate"];
    state.build.approvalId = "approval-exact";
    state.build.catalog = { state: "sealed", digest: `sha256:${"7".repeat(64)}` };
    Object.assign(f.state.record, { state: "approved", approvalId: "approval-exact", catalog: { state: "sealed", approvalId: "approval-exact", catalogDigest: state.build.catalog.digest } });
  }
  if (phase === "published") seal();
  f.query.pipelinesForOwner = async () => new Result({ data: state.pipelines } as never);
  f.query.pipelineForOwner = async () => new Result({ data: state.pipelines } as never);
  f.query.pipelineRunForOwner = async () => new Result({ data: [state.run] } as never);
  f.query.executeNamed = async (name, call) => {
    state.calls.push({ name, call });
    if (name === "releaseBuilds") return releaseResult({ intents: state.saved ? [structuredClone(state.build)] : [] });
    if (name === "releasePrepareBuild") {
      state.saved = true;
      if (state.lostPrepareReply) throw new Error("Source check reply was lost");
    } else {
      if (!call.includes(`intentId: "${intentId}"`)) throw new Error("Unexpected fixture request identity");
      if (name === "releaseGetBuild" && state.failedGet) throw new Error("Release progress unavailable");
      if (name === "releaseStartBuild") {
        Object.assign(state.build, { phase: state.lostStartReply ? "uncertain" : "building", pipelineRunId: state.lostStartReply ? "" : releaseRun.id, workRunId: state.lostStartReply ? "" : releaseRun.workRunId, actions: state.lostStartReply ? ["resume"] : ["observe"] });
        if (state.lostStartReply) throw new Error("Start reply was lost");
      } else if (name === "releaseResumeBuild") {
        if (state.build.phase === "built") { state.build.phase = "ready"; state.build.candidateId = releaseId; state.build.actions = ["approve", "view_candidate"]; f.state.empty = false; }
        else { state.build.phase = "building"; state.build.pipelineRunId = releaseRun.id; state.build.workRunId = releaseRun.workRunId; state.build.actions = ["observe"]; }
      } else if (name === "releasePublishBuild") {
        seal();
        if (state.lostPublishReply) throw new Error("Publication reply was lost");
      } else if (name !== "releaseGetBuild" && name !== "releaseObserveBuild") throw new Error("Unexpected fixture operation");
    }
    if (state.syncApproval && state.build.phase === "ready" && f.state.record.state === "approved") {
      state.build.approvalId = f.state.record.approvalId!; state.build.actions = ["publish", "view_candidate"];
    }
    return releaseResult(structuredClone(state.build));
  };
  return { ...f, journey: state, seal };
}
