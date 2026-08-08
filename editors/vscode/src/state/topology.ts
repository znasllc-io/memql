// Cluster topology, projected from `v1:cluster:node` rows.
//
// Ordinary concept rows -- no bridge involved on the read side. What this
// module adds on top is the two judgements the topology grid renders:
//
//   ORPHAN. A node is an orphan when it is stopped, or when it carries a
//   deploymentId that is not the cluster's current one. The second case is the
//   interesting one: `v1:cluster:node.deploymentId` is stamped at registration
//   from the rollout, so a node still running under a superseded deployment is
//   serving traffic from a release nobody thinks is deployed. That is the
//   condition the reaper (#1874) exists for and the one an operator most needs
//   to see.
//
//   UNDER-REPLICA. A node type running fewer live nodes than the current
//   deployment's `v1:cluster:deploymentNodeSpec.replicas` declares.
//
// RUNNING VERSION -- and the one thing worth knowing before reading further:
// `v1:cluster:node` carries NO version field, and its `labels` map is empty in
// practice (the registerNode automation forwards neither `node.version` nor
// `node.labels`; the startup version lands only on the cluster row). So a
// node's running version is not read, it is RESOLVED: through the node's
// deploymentId to that deployment's per-tier spec, falling back to the
// deployment's own engine version. That is exactly the engine-as-spine model
// the deploymentNodeSpec concept defines, so the answer is correct rather than
// merely available -- but it is derived, and a node whose deploymentId is
// empty (a pre-#1873 registration) genuinely has no resolvable version. The
// renderer draws that as "version unknown" rather than blank, so a derived
// answer is never confused with a missing one.
//
// Lives under src/state/ (not src/webview/) because it holds no VS Code types
// -- see cmd/memql-lsp/vscodeimportrule_test.go, which enforces that
// mechanically.

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  indexDeployments,
  indexSpecsByTier,
  resolveTierVersion,
  tierKey,
  type DeploymentNodeSpec,
  type DeploymentRecord,
} from "./deploymentHistory.js";
import { flattenForList } from "./rowProjection.js";

export const NODE_CONCEPT = "v1:cluster:node";

/**
 * The health states that count as "this node is part of the serving set".
 *
 * `offline` and `stopped` are excluded: a stopped node is already an orphan by
 * the rule above, and an offline one is by definition not serving. `draining`
 * IS counted -- it is still taking traffic, and excluding it would make every
 * ordinary rolling restart report a spurious under-replica alarm for as long
 * as the drain lasts.
 */
const LIVE_HEALTH = new Set(["connecting", "healthy", "degraded", "draining"]);

/** One projected cluster node. */
export interface TopologyNode {
  /** The row id -- what the grid keys tiles by. */
  id: string;
  nodeType: string;
  flavor: string;
  health: string;
  address: string;
  lastSeen: string;
  deploymentId: string;
  /** The resolved running version. Empty when it could not be resolved. */
  version: string;
  orphan: boolean;
  /** Why, in operator-readable prose. Empty when `orphan` is false. */
  orphanReason: string;
  /** True when this node is in the serving set (see LIVE_HEALTH). */
  live: boolean;
}

/** One node type's replica accounting under the current deployment. */
export interface ReplicaTier {
  nodeType: string;
  /** The declared replica count, from the current deployment's spec. 0 when
   *  no spec declares this tier. */
  expected: number;
  /** Live, non-orphaned nodes of this type. */
  running: number;
  under: boolean;
}

export interface Topology {
  nodes: TopologyNode[];
  tiers: ReplicaTier[];
  /** Live node counts keyed by `tierKey(deploymentId, nodeType)`, which is what
   *  the composition preview needs to flag a historical deployment's leftovers. */
  liveByTier: Map<string, number>;
  orphanCount: number;
  underReplicaCount: number;
}

export interface TopologyInputs {
  nodeRows: Row[];
  deployments: DeploymentRecord[];
  specs: DeploymentNodeSpec[];
  /** The current deployment id, from currentDeploymentId(). Empty means
   *  "unknown", which suppresses stale-deployment orphan flagging entirely. */
  currentDeploymentId: string;
}

export function buildTopology(inputs: TopologyInputs): Topology {
  const deployments = indexDeployments(inputs.deployments);
  const specs = indexSpecsByTier(inputs.specs);
  const current = inputs.currentDeploymentId;

  const nodes: TopologyNode[] = inputs.nodeRows.map((raw) => {
    const row = flattenForList(raw);
    const nodeType = str(row, "nodeType");
    const health = str(row, "health");
    const deploymentId = str(row, "deploymentId");
    const { orphan, orphanReason } = classifyOrphan(health, deploymentId, current);
    const resolved = resolveTierVersion(
      specs.get(tierKey(deploymentId, nodeType)),
      deployments.get(deploymentId),
    );
    return {
      id: str(row, "id"),
      nodeType,
      flavor: str(row, "flavor"),
      health,
      address: str(row, "address"),
      lastSeen: str(row, "lastSeen"),
      deploymentId,
      // Only resolvable through a deploymentId. Without one there is no
      // deployment and no spec to resolve against, and reporting the cluster's
      // current version anyway would be a guess presented as a fact.
      version: deploymentId === "" ? "" : resolved.version,
      orphan,
      orphanReason,
      live: LIVE_HEALTH.has(health),
    };
  });

  nodes.sort(
    (a, b) => a.nodeType.localeCompare(b.nodeType) || a.id.localeCompare(b.id),
  );

  const tiers = buildTiers(nodes, inputs.specs, current);
  return {
    nodes,
    tiers,
    liveByTier: countLiveByTier(nodes),
    orphanCount: nodes.filter((n) => n.orphan).length,
    underReplicaCount: tiers.filter((t) => t.under).length,
  };
}

/**
 * Decide whether a node is an orphan, and say why.
 *
 * The stopped check comes FIRST because it is the more specific fact: a
 * stopped node from a superseded deployment is both, and "stopped" is the one
 * that tells an operator the pod is already gone rather than still serving.
 *
 * A node with an EMPTY deploymentId is never a stale-deployment orphan.
 * deploymentId is stamped at registration (#1873) and a node registered before
 * that, or in a context where MEMQL_DEPLOYMENT_ID is unset, simply has no
 * deployment to compare -- flagging it would turn a missing field into an
 * alarm. Likewise, an unknown CURRENT deployment (empty `current`) suppresses
 * the whole rule: with nothing to compare against, marking the entire cluster
 * orphaned on the strength of a failed history read is far worse than saying
 * nothing.
 */
function classifyOrphan(
  health: string,
  deploymentId: string,
  current: string,
): { orphan: boolean; orphanReason: string } {
  if (health === "stopped") {
    return { orphan: true, orphanReason: "node is stopped" };
  }
  if (current !== "" && deploymentId !== "" && deploymentId !== current) {
    return {
      orphan: true,
      orphanReason: `running under deployment ${deploymentId}, which is not the current deployment (${current})`,
    };
  }
  return { orphan: false, orphanReason: "" };
}

/**
 * The replica tally for the CURRENT deployment.
 *
 * Every tier the current deployment declares gets a row, including one with
 * zero running nodes -- a tier that failed to come up at all is the loudest
 * possible under-replica and must not vanish for want of a node to project.
 *
 * Node types that are RUNNING but undeclared also get a row, with expected 0.
 * They can never be flagged under-replica (0 declared, so nothing is missing),
 * but they belong in the tally: a tier nobody declared and yet is running is
 * itself worth seeing.
 */
function buildTiers(
  nodes: TopologyNode[],
  specs: DeploymentNodeSpec[],
  current: string,
): ReplicaTier[] {
  const expected = new Map<string, number>();
  if (current !== "") {
    for (const spec of specs) {
      if (spec.deploymentId === current) expected.set(spec.nodeType, spec.replicas);
    }
  }

  const running = new Map<string, number>();
  for (const node of nodes) {
    if (node.nodeType === "") continue;
    // Orphans are excluded from the running count on purpose: a tier held up
    // entirely by nodes from a superseded release is NOT correctly
    // provisioned, and counting them would hide exactly that.
    const counts = node.live && !node.orphan ? 1 : 0;
    running.set(node.nodeType, (running.get(node.nodeType) ?? 0) + counts);
  }

  const nodeTypes = new Set<string>([...expected.keys(), ...running.keys()]);
  return [...nodeTypes]
    .sort((a, b) => a.localeCompare(b))
    .map((nodeType) => {
      const want = expected.get(nodeType) ?? 0;
      const have = running.get(nodeType) ?? 0;
      return { nodeType, expected: want, running: have, under: want > 0 && have < want };
    });
}

/** Live nodes per (deploymentId, nodeType), for the composition preview. */
function countLiveByTier(nodes: TopologyNode[]): Map<string, number> {
  const out = new Map<string, number>();
  for (const node of nodes) {
    if (!node.live || node.deploymentId === "" || node.nodeType === "") continue;
    const key = tierKey(node.deploymentId, node.nodeType);
    out.set(key, (out.get(key) ?? 0) + 1);
  }
  return out;
}

/** Live node counts by node type for ONE deployment, keyed for composeDeployment. */
export function liveNodeTypesFor(
  topology: Topology,
  deploymentId: string,
): Map<string, number> {
  const out = new Map<string, number>();
  if (deploymentId === "") return out;
  for (const node of topology.nodes) {
    if (!node.live || node.deploymentId !== deploymentId || node.nodeType === "") continue;
    out.set(node.nodeType, (out.get(node.nodeType) ?? 0) + 1);
  }
  return out;
}

/** The human label for a node: its type, plus its flavor when it has one. */
export function nodeLabel(node: TopologyNode): string {
  if (node.nodeType === "") return node.id === "" ? "(unnamed node)" : node.id;
  return node.flavor === "" ? node.nodeType : `${node.nodeType} / ${node.flavor}`;
}

/**
 * Shorten an id for display.
 *
 * memQL ids are canonically `{concept}:{shortId}`, so the trailing segment is
 * the identifying part and the leading concept path is noise in a grid tile.
 * A plain id with no colon (a node id like `bff-local`, a rollout hash) passes
 * through unchanged apart from the length cap.
 */
export function shortenId(id: string, max = 12): string {
  if (id === "") return "";
  const at = id.lastIndexOf(":");
  const tail = at >= 0 ? id.slice(at + 1) : id;
  return tail.length > max ? tail.slice(0, max) : tail;
}

function str(row: Row, key: string): string {
  const value = row[key];
  return typeof value === "string" ? value : "";
}
