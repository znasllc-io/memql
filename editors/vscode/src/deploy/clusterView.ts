// State -> view-kit view models.
//
// The renderers take flat, already-decided view types (booleans and strings);
// this is where the panel's state is turned into them. It lives here rather
// than inside the webview adapter so the WORDING is unit-testable -- the
// difference between "1 of 2" and "2 of 2 [under-replica]" is the entire
// output of the replica tally, and an assertion on it is what proves the
// under-replica rule reached the screen rather than merely existing in
// state/topology.ts.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import type {
  DeploymentHistoryView,
  TallyRowView,
  TopologyNodeView,
} from "@znasllc-io/memql-view-kit";

import { shortenDigest, type CompositionTier, type DeploymentRecord } from "../state/deploymentHistory.js";
import { nodeLabel, shortenId, type Topology, type TopologyNode } from "../state/topology.js";

/** One tile per registered node. */
export function topologyViews(topology: Topology): TopologyNodeView[] {
  return topology.nodes.map((node) => ({
    id: node.id,
    label: nodeLabel(node),
    detail: nodeDetail(node),
    version: node.version,
    deployment: shortenId(node.deploymentId),
    health: node.health,
    orphan: node.orphan,
    orphanReason: node.orphanReason,
  }));
}

// The tile's second line: the short node id, plus the advertised address when
// it adds anything. The address is what an operator needs to reach the pod,
// and the id alone is often just the node type repeated.
function nodeDetail(node: TopologyNode): string {
  const short = shortenId(node.id, 24);
  if (node.address === "" || node.address === short) return short;
  return `${short} -- ${node.address}`;
}

/**
 * The replica tally: one row per node type, flagged when short.
 *
 * A tier with nothing declared reads "N running" rather than "N of 0": "of 0"
 * would look like a violated expectation when in fact no expectation exists.
 */
export function replicaTallyViews(topology: Topology): TallyRowView[] {
  return topology.tiers.map((tier) => ({
    name: tier.nodeType,
    detail: tier.expected === 0 ? "no replica count declared" : "",
    count:
      tier.expected === 0
        ? `${tier.running} running`
        : `${tier.running} of ${tier.expected}`,
    flag: tier.under ? "under-replica" : "",
  }));
}

/**
 * The selected deployment's composition.
 *
 * `(engine)` marks a version that came from the deployment's spine rather than
 * from a per-tier pin -- the deploymentNodeSpec concept treats those as
 * genuinely different states, and rendering them identically would hide that a
 * tier is pinned away from the rest of the release.
 */
export function compositionViews(tiers: CompositionTier[]): TallyRowView[] {
  return tiers.map((tier) => ({
    name: tier.nodeType,
    detail: compositionDetail(tier),
    count: tier.replicas === 0 ? `${tier.liveNodes} live` : `${tier.replicas} replicas`,
    // The flag answers "did this deployment leave anything running?" -- the
    // question an operator opens a historical deployment to ask.
    flag: tier.orphaned ? `${tier.liveNodes} orphaned` : "",
  }));
}

function compositionDetail(tier: CompositionTier): string {
  const parts: string[] = [];
  if (tier.version === "") {
    parts.push("version unknown");
  } else {
    parts.push(tier.versionInherited ? `${tier.version} (engine)` : tier.version);
  }
  const digest = shortenDigest(tier.imageDigest);
  if (digest !== "") parts.push(digest);
  return parts.join(" -- ");
}

/** The history list, newest-first (the caller's records are already ordered). */
export function historyViews(
  records: DeploymentRecord[],
  currentDeploymentId: string,
): DeploymentHistoryView[] {
  return records.map((record) => ({
    id: record.deploymentId,
    version: record.version,
    environment: record.environment,
    provider: record.provider,
    status: record.status,
    // An empty currentDeploymentId means "unknown", and must not accidentally
    // match a record whose id is also empty -- projectDeployments drops those,
    // but the guard keeps the rule local rather than depending on that.
    current: currentDeploymentId !== "" && record.deploymentId === currentDeploymentId,
  }));
}

/** The one-line summary above the grid. */
export function topologySummary(topology: Topology): string {
  const parts = [`${topology.nodes.length} nodes`];
  if (topology.orphanCount > 0) parts.push(`${topology.orphanCount} orphaned`);
  if (topology.underReplicaCount > 0) {
    parts.push(`${topology.underReplicaCount} node types under-replicated`);
  }
  return parts.join(", ");
}
