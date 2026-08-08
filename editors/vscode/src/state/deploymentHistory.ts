// Deployment history and per-tier composition, projected from ordinary
// concept rows.
//
// `v1:cluster:deployment` and `v1:cluster:deploymentNodeSpec` are NOT bridged
// through the deploy-control service and deliberately never will be (memql#3311
// records the decision): they are plain graph rows, readable by the same
// browseConceptPage the concept browser uses. This module turns those rows into
// the small set of facts the DevOps surface renders, and -- more importantly --
// owns the two judgements that surface makes:
//
//   - which deployment is CURRENT, which is what makes every other node an
//     orphan (see currentDeploymentId below), and
//   - which memQL version a given node type actually runs, which is the
//     engine-as-spine resolution the deploymentNodeSpec concept describes.
//
// Both live here rather than in the renderer or the panel so the VS Code tab
// and the portal reach the same answer by sharing the implementation, not by
// each re-deriving it.
//
// Lives under src/state/ (not src/webview/) because it holds no VS Code types:
// src/views/ and src/webview/ are the only adapter layers allowed to import
// `vscode`, and everything carrying logic stays out of them so it is testable
// under bare `node --test`. Enforced mechanically -- see
// cmd/memql-lsp/vscodeimportrule_test.go.

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { flattenForList } from "./rowProjection.js";

/** The concept ids this module projects. */
export const DEPLOYMENT_CONCEPT = "v1:cluster:deployment";
export const DEPLOYMENT_NODE_SPEC_CONCEPT = "v1:cluster:deploymentNodeSpec";

/** One deployment record, flattened out of a v1:cluster:deployment row. */
export interface DeploymentRecord {
  /** The stable deploy id, also the Argo rollout hash. The handle every
   *  deploy-control action takes. */
  deploymentId: string;
  status: string;
  version: string;
  environment: string;
  provider: string;
  region: string;
  imageDigest: string;
  /** The deploy-start time (the row intrinsic). */
  createdAt: string;
  /** The most recent status transition. */
  updatedAt: string;
  triggeredBy: string;
  previousDeploymentId: string;
}

/** One (deployment, nodeType) spec row. */
export interface DeploymentNodeSpec {
  deploymentId: string;
  nodeType: string;
  /** Empty means "engine as spine" -- resolve against the parent deployment's
   *  version rather than pinning this node type. See resolveTierVersion. */
  version: string;
  replicas: number;
  imageDigest: string;
  updatedAt: string;
}

/**
 * The statuses that mean "this deployment actually reached the cluster".
 *
 * `succeeded` is the forward-deploy terminal state. `rolled_back` is the
 * ROLLBACK's terminal state, and it is live rather than historical: a rollback
 * creates a NEW deployment record pointing at the historical digest, and the
 * driveDeploymentInProgress automation lands that record in `rolled_back`
 * (deployment-console.md, "Terminal-status parity", #2168). Treating
 * `rolled_back` as not-current would therefore mark every node in the cluster
 * an orphan for the entire life of a rolled-back release -- the single most
 * misleading thing this panel could say, and precisely when an operator is
 * least able to afford it.
 *
 * `superseded` is explicitly "a newer deploy replaced me". `failed` never
 * landed. `pending` / `in_progress` have not landed YET, which is why a deploy
 * in flight leaves the previous deployment current and its nodes un-orphaned.
 */
const LANDED_STATUSES = new Set(["succeeded", "rolled_back"]);

/**
 * Project raw `v1:cluster:deployment` rows into records, newest deploy first.
 *
 * The concept is append-only: every status transition appends a payload
 * version under the same row id, so the same deploymentId can legitimately
 * appear more than once in a page. Only the newest payload survives, chosen by
 * `updatedAt` (the transition clock) and falling back to `createdAt`.
 *
 * ORDERING is by `createdAt`, NOT `updatedAt`, and the two are different
 * choices with different consequences: `createdAt` is when the deploy was cut,
 * which is stable, so a deployment does not jump up the list every time its
 * status advances. A list that re-orders itself under a watching operator is
 * one they stop trusting.
 */
export function projectDeployments(rows: Row[]): DeploymentRecord[] {
  const newest = new Map<string, DeploymentRecord>();
  for (const raw of rows) {
    const record = toDeploymentRecord(raw);
    if (record.deploymentId === "") continue;
    const existing = newest.get(record.deploymentId);
    if (existing === undefined || transitionClock(record) >= transitionClock(existing)) {
      newest.set(record.deploymentId, record);
    }
  }
  return [...newest.values()].sort((a, b) => {
    const byCreated = compareDesc(a.createdAt, b.createdAt);
    // Two records cut in the same instant (a fixture, or a coarse clock) still
    // need a total order, or the list shuffles between renders for no reason
    // an operator can see.
    return byCreated !== 0 ? byCreated : a.deploymentId.localeCompare(b.deploymentId);
  });
}

/** Project raw `v1:cluster:deploymentNodeSpec` rows, newest spec per tier. */
export function projectNodeSpecs(rows: Row[]): DeploymentNodeSpec[] {
  const newest = new Map<string, DeploymentNodeSpec>();
  for (const raw of rows) {
    const spec = toNodeSpec(raw);
    if (spec.deploymentId === "" || spec.nodeType === "") continue;
    const key = tierKey(spec.deploymentId, spec.nodeType);
    const existing = newest.get(key);
    if (existing === undefined || spec.updatedAt >= existing.updatedAt) {
      newest.set(key, spec);
    }
  }
  return [...newest.values()].sort(
    (a, b) =>
      a.deploymentId.localeCompare(b.deploymentId) || a.nodeType.localeCompare(b.nodeType),
  );
}

/**
 * The deployment the cluster is currently running: the most recently CUT
 * deployment that actually landed (see LANDED_STATUSES).
 *
 * Empty when nothing has landed -- a brand-new cluster, or a history page that
 * has not loaded. An empty answer is load-bearing rather than a degenerate
 * case: buildTopology treats it as "the current deployment is unknown" and
 * flags NOTHING as a stale-deployment orphan, because marking every node in
 * the cluster an orphan on the strength of a failed read would be worse than
 * saying nothing.
 *
 * Takes an already-sorted record list (projectDeployments' output) but does not
 * rely on the sort: it scans for the newest landed record itself, so a caller
 * that filtered or re-ordered the list cannot silently get a wrong answer.
 */
export function currentDeploymentId(records: DeploymentRecord[]): string {
  let best: DeploymentRecord | undefined;
  for (const record of records) {
    if (!LANDED_STATUSES.has(record.status)) continue;
    if (best === undefined) {
      best = record;
      continue;
    }
    // The same total order projectDeployments sorts by, applied independently
    // so this answer does not depend on the caller having preserved that sort.
    const byCreated = compareDesc(record.createdAt, best.createdAt);
    const newer =
      byCreated !== 0 ? byCreated < 0 : record.deploymentId.localeCompare(best.deploymentId) < 0;
    if (newer) best = record;
  }
  return best?.deploymentId ?? "";
}

/**
 * Resolve the memQL version a tier runs under a deployment.
 *
 * Engine-as-spine, exactly as the deploymentNodeSpec concept defines it: a
 * non-empty per-tier `version` pins that node type; an empty one resolves
 * against the parent deployment's engine version. Returning the deployment's
 * version for the empty case (rather than "") is what lets the topology grid
 * answer "which release is this node running" for the overwhelmingly common
 * cluster where no tier is individually pinned.
 *
 * `inherited` tells the caller which of the two happened, so a composition
 * preview can say so instead of making a pin and the spine look identical.
 */
export function resolveTierVersion(
  spec: DeploymentNodeSpec | undefined,
  deployment: DeploymentRecord | undefined,
): { version: string; inherited: boolean } {
  if (spec !== undefined && spec.version !== "") {
    return { version: spec.version, inherited: false };
  }
  return { version: deployment?.version ?? "", inherited: true };
}

/** One node type's slice of a deployment. */
export interface CompositionTier {
  nodeType: string;
  /** The resolved version -- the tier's own pin, or the deployment's spine. */
  version: string;
  /** True when `version` came from the deployment rather than a per-tier pin. */
  versionInherited: boolean;
  replicas: number;
  imageDigest: string;
  /** How many registered nodes currently carry this deploymentId + nodeType. */
  liveNodes: number;
  /**
   * True when this tier still has live nodes but the deployment is NOT the
   * current one -- i.e. those nodes are orphans from a superseded release.
   * This is the "with orphans flagged" half of the composition preview.
   */
  orphaned: boolean;
}

/**
 * Build the per-tier composition preview for one deployment.
 *
 * `liveNodeTypes` counts registered nodes by node type for THIS deploymentId
 * (buildTopology produces it). A tier is flagged when the deployment is not
 * current and nodes are still running under it -- which is the thing an
 * operator opening a historical deployment most wants to know: "did this one
 * leave anything behind?"
 */
export function composeDeployment(
  deployment: DeploymentRecord | undefined,
  specs: DeploymentNodeSpec[],
  liveNodeTypes: ReadonlyMap<string, number>,
  isCurrent: boolean,
): CompositionTier[] {
  if (deployment === undefined) return [];
  const mine = specs.filter((s) => s.deploymentId === deployment.deploymentId);

  // Node types with live nodes but NO spec row still belong in the preview.
  // A spec-less tier is exactly the case where something was deployed outside
  // the per-tier model (or the spec write failed), and omitting it would hide
  // running nodes from the one view that is supposed to account for them.
  const seen = new Set(mine.map((s) => s.nodeType));
  const tiers: CompositionTier[] = mine.map((spec) => {
    const resolved = resolveTierVersion(spec, deployment);
    const liveNodes = liveNodeTypes.get(spec.nodeType) ?? 0;
    return {
      nodeType: spec.nodeType,
      version: resolved.version,
      versionInherited: resolved.inherited,
      replicas: spec.replicas,
      imageDigest: spec.imageDigest,
      liveNodes,
      orphaned: !isCurrent && liveNodes > 0,
    };
  });

  for (const [nodeType, liveNodes] of liveNodeTypes) {
    if (seen.has(nodeType) || liveNodes === 0) continue;
    const resolved = resolveTierVersion(undefined, deployment);
    tiers.push({
      nodeType,
      version: resolved.version,
      versionInherited: resolved.inherited,
      // 0, not liveNodes: this is the DECLARED replica count, and the honest
      // answer for a tier with no spec row is that none was declared. Copying
      // the live count here would make an undeclared tier look correctly
      // provisioned.
      replicas: 0,
      imageDigest: "",
      liveNodes,
      orphaned: !isCurrent && liveNodes > 0,
    });
  }

  return tiers.sort((a, b) => a.nodeType.localeCompare(b.nodeType));
}

/** Shorten a `sha256:...` digest for display. Empty stays empty. */
export function shortenDigest(digest: string): string {
  if (digest === "") return "";
  const body = digest.startsWith("sha256:") ? digest.slice("sha256:".length) : digest;
  return body.length > 12 ? body.slice(0, 12) : body;
}

/** Index specs by (deploymentId, nodeType) for the topology's version lookup. */
export function indexSpecsByTier(
  specs: DeploymentNodeSpec[],
): Map<string, DeploymentNodeSpec> {
  const out = new Map<string, DeploymentNodeSpec>();
  for (const spec of specs) out.set(tierKey(spec.deploymentId, spec.nodeType), spec);
  return out;
}

/** Index deployments by deploymentId. */
export function indexDeployments(
  records: DeploymentRecord[],
): Map<string, DeploymentRecord> {
  const out = new Map<string, DeploymentRecord>();
  for (const record of records) out.set(record.deploymentId, record);
  return out;
}

export function tierKey(deploymentId: string, nodeType: string): string {
  // A newline separator rather than ":" -- a deploymentId is an opaque hash
  // and a canonical memQL id contains colons, so ":" could make two distinct
  // (deployment, tier) pairs collide onto one key.
  return `${deploymentId}\n${nodeType}`;
}

// -----------------------------------------------------------------------------
// Row -> record
// -----------------------------------------------------------------------------

function toDeploymentRecord(raw: Row): DeploymentRecord {
  const row = flattenForList(raw);
  return {
    // The payload field is authoritative: it is what Deploy /
    // RollbackDeployment take as their handle. The row intrinsic is a
    // fallback for a row written without it, and would otherwise leave a
    // record present in the list but impossible to act on.
    deploymentId: str(row, "deploymentId") || str(row, "id"),
    status: str(row, "status"),
    version: str(row, "version"),
    environment: str(row, "environment"),
    provider: str(row, "provider"),
    region: str(row, "region"),
    imageDigest: str(row, "imageDigest"),
    createdAt: str(row, "createdAt"),
    updatedAt: str(row, "updatedAt"),
    triggeredBy: str(row, "triggeredBy"),
    previousDeploymentId: str(row, "previousDeploymentId"),
  };
}

function toNodeSpec(raw: Row): DeploymentNodeSpec {
  const row = flattenForList(raw);
  return {
    deploymentId: str(row, "deploymentId"),
    nodeType: str(row, "nodeType"),
    version: str(row, "version"),
    replicas: num(row, "replicas"),
    imageDigest: str(row, "imageDigest"),
    updatedAt: str(row, "updatedAt") || str(row, "createdAt"),
  };
}

function str(row: Row, key: string): string {
  const value = row[key];
  return typeof value === "string" ? value : "";
}

function num(row: Row, key: string): number {
  const value = row[key];
  if (typeof value === "number" && Number.isFinite(value)) return value;
  // protojson renders an int64 as a STRING, so a replicas count can arrive
  // either way depending on the field's declared width. Parsing the string
  // form is not defensive padding -- it is the difference between "2 of 2" and
  // an expected count of 0 that never flags an under-replicated tier.
  if (typeof value === "string" && value !== "") {
    const parsed = Number(value);
    if (Number.isFinite(parsed)) return parsed;
  }
  return 0;
}

/** The clock a record's own timeline is collapsed by. */
function transitionClock(record: DeploymentRecord): string {
  return record.updatedAt !== "" ? record.updatedAt : record.createdAt;
}

/**
 * Descending comparison over RFC3339 timestamps.
 *
 * String comparison, not Date parsing: RFC3339 with a fixed offset sorts
 * lexicographically, and an unparseable or empty value degrades to "oldest"
 * rather than to NaN, which would make the sort non-deterministic.
 */
function compareDesc(a: string, b: string): number {
  if (a === b) return 0;
  return a > b ? -1 : 1;
}
