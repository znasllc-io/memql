import { Result } from "@znasllc-io/memql-sdk-core/client";
import type { Target } from "../../src/apps/cluster/releases/model";
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
export function releaseFixture(initial = "ready") {
  const state = { record: candidateRecord(initial), publications: [] as object[], targets: [releaseTarget] as Target[], empty: false, failedRead: false, lostPublishReply: false, calls: [] as { kind: string; args: unknown }[] };
  const query: ReleaseQueries = {
    releaseCandidates: async (args) => { state.calls.push({ kind: "list", args }); if (state.failedRead) throw new Error("Release journal unavailable"); return releaseResult({ candidates: state.empty ? [] : [{ candidateId: releaseId, state: state.record.state, components: state.record.manifest.components, createdAt: "2026-10-06T18:00:00Z", evidenceCount: 1 }] }); },
    releaseGetCandidate: async (args) => { state.calls.push({ kind: "get", args }); if (state.failedRead) throw new Error("Release journal unavailable"); return releaseResult(state.record); },
    releaseCandidateConfiguration: async () => releaseResult({ targets: state.targets }),
    releaseCandidatePublications: async () => releaseResult({ candidateId: releaseId, publications: state.publications }),
    releaseApproveCandidate: async (args) => { state.calls.push({ kind: "approve", args }); state.record = candidateRecord("approved"); return releaseResult(state.record); },
    releaseRetireCandidate: async (args) => { state.calls.push({ kind: "retire", args }); state.record = candidateRecord("retired"); return releaseResult(state.record); },
    releasePrepareCandidate: async (args) => { state.calls.push({ kind: "prepare", args }); state.record = candidateRecord("ready"); return releaseResult(state.record); },
    releasePublishCandidate: async (args) => {
      state.calls.push({ kind: "publish", args });
      const p = { candidateId: releaseId, approvalId: "approval-exact", effectId: "durable-publication", target: releaseTarget, artifact: state.record.manifest.components[0]!.artifacts[0]!, state: state.lostPublishReply ? "pending" : "complete" };
      state.publications = [p];
      if (state.lostPublishReply) throw new Error("Publication reply was lost");
      return releaseResult(p);
    },
  };
  return { state, query };
}
