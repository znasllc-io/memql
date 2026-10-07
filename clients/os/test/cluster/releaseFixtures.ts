import { Result } from "@znasllc-io/memql-sdk-core/client";
import type { Draft, Target } from "../../src/apps/cluster/releases/model";
import type { ReleaseQueries } from "../../src/apps/cluster/releases/ReleasesSection";

export const releaseId = `sha256:${"a".repeat(64)}`;
export const targetDigest = `sha256:${"b".repeat(64)}`;
export const releaseTarget = { targetId: "rehearsal", targetDigest, component: "engine", artifact: "bff", operation: "publish" as const, origin: "https://registry.example.test", repository: "memql/bff", kind: "oci" as const };
export function candidateRecord(state = "ready") {
  return { candidateId: releaseId, state, ...(state === "approved" ? { approvalId: "approval-exact" } : {}), manifest: {
    formatVersion: 1, ownerUserId: "owner", workflowDigest: `sha256:${"f".repeat(64)}`,
    components: [{ name: "engine", version: "0.25.0", repository: "example/memql", commit: "d".repeat(40), artifacts: [{ name: "bff", kind: "oci", platform: "linux/arm64", digest: `sha256:${"c".repeat(64)}`, imageDigest: `sha256:${"e".repeat(64)}`, size: 1853440 }] }],
    compatibility: [], destinations: [releaseTarget],
    evidence: [{ name: "engine-tests", component: "engine", workRunId: "run-1", stepKey: "tests.engine", attempt: 1, receiptDigest: `sha256:${"8".repeat(64)}` }],
  } };
}
export function releaseResult(value: object) { return new Result({ data: [value] } as never); }
export const buildRun = { id: "pipeline-1", status: "completed", conclusion: "success", mode: "full", workRunId: "work-build-1", repository: "example/memql", sha: "d".repeat(40), title: "Build engine 0.25.0", finishedAt: "2026-10-06T18:00:00Z" };
export function releaseFixture(initial = "ready") {
  const state = { record: candidateRecord(initial), publications: [] as object[], targets: [releaseTarget] as Target[], drafts: [] as Draft[], empty: false, failedRead: false, failedDraftRead: false, lostPublishReply: false, lostDraftReply: false, lostPromotionReply: false, lostAssemblyReply: false,
    sources: [{ component: "engine", repository: "example/memql", path: "VERSION" }],
    assemblies: [{ name: "engine-release", components: [{ name: "engine", runs: ["build"], artifacts: [{ name: "bff", run: "build", stepKey: "build.bff", path: "bff.tar", kind: "oci", platform: "linux/arm64" }] }], targets: ["rehearsal"], compatibility: [] }],
    runs: [buildRun], calls: [] as { kind: string; args: unknown }[] };
  const query: ReleaseQueries = {
    releaseCandidates: async (args) => { state.calls.push({ kind: "list", args }); if (state.failedRead) throw new Error("Release journal unavailable"); return releaseResult({ candidates: state.empty ? [] : [{ candidateId: releaseId, state: state.record.state, components: state.record.manifest.components, createdAt: "2026-10-06T18:00:00Z", evidenceCount: 1 }] }); },
    releaseGetCandidate: async (args) => { state.calls.push({ kind: "get", args }); if (state.failedRead) throw new Error("Release journal unavailable"); return releaseResult(state.record); },
    releaseCandidateConfiguration: async () => releaseResult({ targets: state.targets, sources: state.sources, assemblies: state.assemblies }),
    releaseCandidatePublications: async () => releaseResult({ candidateId: releaseId, publications: state.publications }),
    releaseApproveCandidate: async (args) => { state.calls.push({ kind: "approve", args }); Object.assign(state.record, { state: "approved", approvalId: "approval-exact" }); return releaseResult(state.record); },
    releaseRetireCandidate: async (args) => { state.calls.push({ kind: "retire", args }); state.record.state = "retired"; return releaseResult(state.record); },
    releasePrepareCandidate: async (args) => { state.calls.push({ kind: "prepare", args }); state.record.state = "ready"; return releaseResult(state.record); },
    releaseCandidateDrafts: async () => { if (state.failedDraftRead) throw new Error("Draft history unavailable"); return releaseResult({ candidateId: releaseId, drafts: state.drafts }); },
    releaseCreateCandidateDraft: async (args) => {
      state.calls.push({ kind: "draft", args });
      state.drafts = [{ intentId: "draft-intent", candidateId: releaseId, approvalId: "approval-exact", state: state.lostDraftReply ? "creating" : "ready", targets: [args.targetId], ...(state.lostDraftReply ? {} : { releaseId: 81 }) }];
      if (state.lostDraftReply) throw new Error("Draft creation reply was lost");
      return releaseResult(state.drafts[0]!);
    },
    releasePromoteCandidateDraft: async (args) => {
      state.calls.push({ kind: "promote", args });
      state.drafts = [{ ...state.drafts[0]!, state: state.lostPromotionReply ? "promoting" : "published" }];
      if (state.lostPromotionReply) throw new Error("Public publication reply was lost");
      return releaseResult(state.drafts[0]!);
    },
    releaseAssembleCandidate: async (args) => { state.calls.push({ kind: "assemble", args }); state.empty = false; if (state.lostAssemblyReply) throw new Error("Preparation reply was lost"); return releaseResult(state.record); },
    pipelineRunsForOwner: async (args) => { state.calls.push({ kind: "runs", args }); return new Result({ data: state.runs } as never); },
    releasePublishCandidate: async (args) => {
      state.calls.push({ kind: "publish", args });
      const p = { candidateId: releaseId, approvalId: "approval-exact", effectId: "durable-publication", target: state.targets.find((t) => t.targetId === args.targetId), artifact: state.record.manifest.components[0]!.artifacts[0]!, state: state.lostPublishReply ? "pending" : "complete" };
      state.publications = [p];
      if (state.lostPublishReply) throw new Error("Publication reply was lost");
      return releaseResult(p);
    },
  };
  return { state, query };
}
export function fileReleaseFixture(initial = "approved") {
  const fixture = releaseFixture(initial);
  Object.assign(fixture.state.record.manifest.components[0]!.artifacts[0]!, { kind: "file", imageDigest: "", platform: "darwin/arm64" });
  fixture.state.targets = [{ ...releaseTarget, kind: "file", tag: "v0.25.0", sourceCommit: "d".repeat(40), assetName: "MemQL.zip", draft: { name: "MemQL 0.25.0", body: "Verified desktop release.\nIncludes the reviewed fixes.", prerelease: false, latest: true } }];
  return fixture;
}
