// The four sources that can say what release a cluster is running.
//
// Each collector answers for ONE source and returns "" for "nothing to say".
// NONE of them throws: a version refresh is opportunistic background work
// triggered by a surface being looked at, and every source here is a
// subprocess, an RPC or a socket that can fail on a plane. A failure is a
// version we did not learn, never an error in front of an operator.
//
// None of them judges its own answer either. A branch name from ArgoCD and the
// build stamp from a live connection are both reported verbatim; refusing to
// let them overwrite a recorded release is `learners.ts`'s decision, and
// keeping that judgement in ONE place is what makes it testable and what stops
// two collectors from disagreeing about what counts.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
// The two sources that need a live cluster arrive as injected closures rather
// than as clients, so this module stays pure and the wiring stays in one place.

import type { ClusterConfig } from "../clusters/model.js";
import { receiptNamesAnotherCluster } from "../clusters/ownershipRoute.js";
import { recordedDomain, recordedStackTag, type Receipt } from "../install/receipt.js";
import { capabilityScriptPath, type ScriptOutcome } from "../install/runner.js";
import type { VersionCandidate } from "./learners.js";

/** The capability whose envelope publishes `argocdTargetRevision`. */
export const STATUS_CAPABILITY = "k3d.status";

/** How long the status script is given before a refresh gives up on it. */
export const STATUS_TIMEOUT_MS = 20_000;

/** What deploy-control reports, narrowed to the two fields that name a version. */
export interface DeployVersionStatus {
  version: string;
  engineVersion: string;
}

export interface VersionCollectorDeps {
  /** The checkout the capability scripts live in. */
  repoRoot: string;
  /** The install receipt on THIS machine, or null when there is none. */
  readReceipt: () => Promise<Receipt | null>;
  /** Runs a capability script. Bound to `runCapabilityScript` by the caller. */
  runCapability: (run: {
    scriptPath: string;
    params: Record<string, string>;
    capability?: string;
    timeoutMs?: number;
  }) => Promise<ScriptOutcome>;
  /**
   * `GetDeploymentStatus` for this cluster, when a connection exists that can
   * make the call. Absent means "do not ask" rather than "asked and got
   * nothing" -- the RPC is owner/admin gated, so a reader legitimately has no
   * way to reach it.
   */
  deployStatus?: (cluster: ClusterConfig) => Promise<DeployVersionStatus | null>;
  /** `memqlVersion()` over a live connection, when one is open. */
  liveVersion?: (cluster: ClusterConfig) => Promise<string>;
}

// Every collector funnels through this: a source that fails contributes
// nothing, and the reason dies here rather than propagating. There is no
// logging seam on purpose -- four sources failing quietly on an offline laptop
// is the NORMAL case, and a log line per source per refresh would be noise
// that trains an operator to ignore the channel.
async function quietly(read: () => Promise<string>): Promise<string> {
  try {
    return (await read()).trim();
  } catch {
    return "";
  }
}

async function fromReceipt(
  cluster: ClusterConfig,
  deps: VersionCollectorDeps,
): Promise<string> {
  return quietly(async () => {
    const receipt = await deps.readReceipt();
    if (receipt === null) return "";
    // There is ONE receipt per machine and it names the cluster it installed.
    // Letting it speak for a different cluster would record a version that
    // machine never ran. This refuses on a CONTRADICTION and not on a gap --
    // an unrecorded domain still speaks, which is what the predicate's own
    // callers already rely on.
    if (receiptNamesAnotherCluster(cluster.domain ?? "", recordedDomain(receipt))) return "";
    return recordedStackTag(receipt);
  });
}

async function fromArgocd(
  cluster: ClusterConfig,
  deps: VersionCollectorDeps,
): Promise<string> {
  // `k3d.status` inspects a k3d cluster on THIS machine. Running it while the
  // surface is pointed at a remote cluster would report the local cluster's
  // revision as if it were the remote one's -- a confidently wrong answer,
  // which is worse than no answer.
  if (cluster.local !== true) return "";
  return quietly(async () => {
    const outcome = await deps.runCapability({
      scriptPath: capabilityScriptPath(STATUS_CAPABILITY, deps.repoRoot),
      params: {},
      capability: STATUS_CAPABILITY,
      timeoutMs: STATUS_TIMEOUT_MS,
    });
    const envelope = outcome.envelope;
    if (envelope === null || !envelope.ok) return "";
    const revision = envelope.result["argocdTargetRevision"];
    return typeof revision === "string" ? revision : "";
  });
}

async function fromDeployControl(
  cluster: ClusterConfig,
  deps: VersionCollectorDeps,
): Promise<string> {
  const ask = deps.deployStatus;
  if (ask === undefined) return "";
  return quietly(async () => {
    const status = await ask(cluster);
    if (status === null) return "";
    // `DeploymentStatus.version` is parsed from the overlay's promotion
    // comment and is documented as empty where no such provenance exists. The
    // engine version off the resolved lockfile is the answer there.
    return status.version.trim() !== "" ? status.version : status.engineVersion;
  });
}

async function fromLive(cluster: ClusterConfig, deps: VersionCollectorDeps): Promise<string> {
  const ask = deps.liveVersion;
  if (ask === undefined) return "";
  return quietly(() => ask(cluster));
}

/**
 * Asks every source, concurrently, and returns what answered.
 *
 * Concurrent because they are independent and slow in different ways -- a
 * subprocess, two network round trips, a file read -- and serialising them
 * would make a refresh take the sum rather than the max.
 */
export function createVersionCollector(
  deps: VersionCollectorDeps,
): (cluster: ClusterConfig) => Promise<VersionCandidate[]> {
  return async (cluster) => {
    const [receipt, argocd, deployControl, live] = await Promise.all([
      fromReceipt(cluster, deps),
      fromArgocd(cluster, deps),
      fromDeployControl(cluster, deps),
      fromLive(cluster, deps),
    ]);
    return (
      [
        { source: "receipt", value: receipt },
        { source: "argocd", value: argocd },
        { source: "deployControl", value: deployControl },
        { source: "live", value: live },
      ] as const
    )
      .filter((c) => c.value !== "")
      .map((c) => ({ source: c.source, value: c.value }));
  };
}
