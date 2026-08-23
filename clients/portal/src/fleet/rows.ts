import {
  rowArray,
  rowNumber,
  rowObject,
  rowString,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import { labelMapFromRow, mergeLabels, type LabelMap, type MergedLabel } from "./labels";

// The wire rows, projected into the shapes the Fleet screens render.
//
// PURE, and separate from the hooks, for the reason src/deploy/rows.ts and
// src/me/rows.ts are: a projection asserted through render() and waitFor() is
// asserted through three layers that can each fail for unrelated reasons.
// Everything here is a function of a Row and is unit-testable with no browser,
// no server and no React.
//
// Every read goes through the SDK's typed field helpers rather than indexing
// the map directly: a field the shape did not project comes back as the type's
// zero rather than as `undefined` rendered into the page.

function stringList(row: Row | null, key: string): string[] {
  const raw = rowArray(row, key) ?? [];
  return raw.filter((entry): entry is string => typeof entry === "string");
}

function numberMap(row: Row | null, key: string): Record<string, number> {
  const raw = rowObject(row, key);
  if (raw === null) return {};
  const out: Record<string, number> = {};
  for (const [k, v] of Object.entries(raw)) {
    if (typeof v === "number") out[k] = v;
    else if (typeof v === "string" && v.trim() !== "" && Number.isFinite(Number(v))) out[k] = Number(v);
  }
  return out;
}

function nestedString(nested: Record<string, unknown> | null, key: string): string {
  const v = nested?.[key];
  return typeof v === "string" ? v : "";
}

// ---------------------------------------------------------------------------
// A machine
// ---------------------------------------------------------------------------

export interface Machine {
  id: string;
  ownerUserId: string;
  // What the cockpit reported -- a hostname by default, and RE-STAMPED on
  // every reconnect, which is why it is not the name the page shows.
  name: string;
  // What the owner called it. Empty until renamed.
  displayName: string;
  // The name to render: displayName falling back to name. One derivation, so
  // the list, the detail and the confirm dialog cannot disagree about what a
  // machine is called.
  label: string;
  capabilities: string[];
  os: string;
  arch: string;
  hostname: string;
  // quartz / x11 / wayland / none, from the capability descriptor. Empty when
  // the machine's build predates the descriptor -- which is a fact worth
  // rendering as "not reported" rather than as "none".
  displayServer: string;
  computerUseAvailable: boolean;
  reportedLabels: LabelMap;
  operatorLabels: LabelMap;
  mergedLabels: MergedLabel[];
  // Per-capability parallelism cap, e.g. {HEADLESS: 8, COMPUTERUSE: 1}.
  concurrency: Record<string, number>;
  // Calls in flight as of the machine's most recent heartbeat. Up to one
  // interval stale by construction -- a routing input, never a correctness one.
  activeCount: number;
  lastSeenAt: string;
  // MEMQL_NODE_ID of the replica holding this machine's stream. Empty means no
  // replica holds it.
  connectedNodeId: string;
  lastSelectedAt: string;
  version: string;
  buildTag: string;
  registeredAt: string;
  revokedAt: string;
  revokeReason: string;
}

export function machineFromRow(row: Row): Machine {
  const platform = rowObject(row, "platformInfo");
  const descriptor = rowObject(row, "capabilityDescriptor");
  const reportedLabels = labelMapFromRow(row, "labels");
  const operatorLabels = labelMapFromRow(row, "operatorLabels");
  const name = rowString(row, "name");
  const displayName = rowString(row, "displayName");

  return {
    id: rowString(row, "id"),
    ownerUserId: rowString(row, "ownerUserId"),
    name,
    displayName,
    label: displayName.trim() === "" ? name : displayName,
    capabilities: stringList(row, "capabilities"),
    // platformInfo is the register-time snapshot; the descriptor repeats the
    // platform for machines that send one. platformInfo is preferred because
    // every machine has it.
    os: nestedString(platform, "os") || nestedString(descriptor, "platform"),
    arch: nestedString(platform, "arch"),
    hostname: nestedString(platform, "hostname"),
    displayServer: nestedString(descriptor, "displayServer"),
    computerUseAvailable: descriptor?.["computerUseAvailable"] === true,
    reportedLabels,
    operatorLabels,
    mergedLabels: mergeLabels(reportedLabels, operatorLabels),
    concurrency: numberMap(row, "concurrency"),
    activeCount: rowNumber(row, "activeCount"),
    lastSeenAt: rowString(row, "lastSeenAt"),
    connectedNodeId: rowString(row, "connectedNodeId"),
    lastSelectedAt: rowString(row, "lastSelectedAt"),
    version: rowString(row, "version"),
    buildTag: rowString(row, "buildTag"),
    registeredAt: rowString(row, "registeredAt"),
    revokedAt: rowString(row, "revokedAt"),
    revokeReason: rowString(row, "revokeReason"),
  };
}

// ---------------------------------------------------------------------------
// The routing policy
// ---------------------------------------------------------------------------

// The closed strategy set, in the order the editor offers them: the pre-policy
// default first, then the three that need a reason.
export const ROUTING_STRATEGIES = ["firstFit", "roundRobin", "leastLoaded", "labelMatch"] as const;
export type RoutingStrategy = (typeof ROUTING_STRATEGIES)[number];

export const ROUTING_FALLBACKS = ["none", "nextMatching"] as const;
export type RoutingFallback = (typeof ROUTING_FALLBACKS)[number];

// What each value MEANS, in the operator's terms rather than the schema's.
// Rendered beside the control, because "leastLoaded" does not say what it is
// least-loaded against.
export const STRATEGY_BLURB: Record<RoutingStrategy, string> = {
  firstFit: "Registration order. What the router did before policies existed.",
  roundRobin: "Longest since last chosen first, so two replicas rotate the same way with no shared counter.",
  leastLoaded: "Fewest calls in flight first, against each capability's own cap.",
  labelMatch: "Most preferred labels matched first, then registration order.",
};

export const FALLBACK_BLURB: Record<RoutingFallback, string> = {
  none: "Report the refusal.",
  nextMatching: "Try the next candidate. Only ever before a call has started -- never a re-run.",
};

export interface RoutingPolicy {
  id: string;
  ownerUserId: string;
  strategy: string;
  requireLabels: LabelMap;
  preferLabels: LabelMap;
  fallback: string;
  active: boolean;
  createdAt: string;
}

export function routingPolicyFromRow(row: Row): RoutingPolicy {
  return {
    id: rowString(row, "id"),
    ownerUserId: rowString(row, "ownerUserId"),
    strategy: rowString(row, "strategy"),
    requireLabels: labelMapFromRow(row, "requireLabels"),
    preferLabels: labelMapFromRow(row, "preferLabels"),
    fallback: rowString(row, "fallback"),
    active: row["active"] === true,
    createdAt: rowString(row, "createdAt"),
  };
}

// The policy the editor edits: the newest ACTIVE row, or nothing.
//
// myRoutingPolicies already sorts newest first, so "the first active row" is
// the same choice routingPolicyForOwner makes server-side. That agreement is
// the point: an editor that picked a different row from the one the router
// reads would write its edits somewhere nothing dispatches through.
export function activePolicy(policies: readonly RoutingPolicy[]): RoutingPolicy | null {
  return policies.find((policy) => policy.active) ?? null;
}

// ---------------------------------------------------------------------------
// An invocation, and the routing record on it
// ---------------------------------------------------------------------------

export interface RoutingRecord {
  policyId: string;
  strategy: string;
  candidatesConsidered: string[];
  attempts: number;
  selectedBy: string;
  reroutedFrom: string;
  requireLabels: LabelMap;
  preferLabels: LabelMap;
  // False for a row written before the router existed, and for a path that
  // never picked (a denial before the choice). Rendered as "not recorded"
  // rather than as an empty routing table, which would read as "chose nothing".
  present: boolean;
}

export interface Invocation {
  id: string;
  createdAt: string;
  tool: string;
  action: string;
  outcome: string;
  durationMs: number;
  errorCode: string;
  errorMessage: string;
  routing: RoutingRecord;
}

function routingFromRow(row: Row): RoutingRecord {
  const raw = rowObject(row, "routing");
  const candidates = Array.isArray(raw?.["candidatesConsidered"])
    ? (raw["candidatesConsidered"] as unknown[]).filter(
        (entry): entry is string => typeof entry === "string",
      )
    : [];
  const attemptsRaw = raw?.["attempts"];
  const labelsAt = (key: string): LabelMap => {
    const nested = raw?.[key];
    if (nested === null || typeof nested !== "object" || Array.isArray(nested)) return {};
    const out: LabelMap = {};
    for (const [k, v] of Object.entries(nested as Record<string, unknown>)) {
      if (typeof v === "string") out[k] = v;
    }
    return out;
  };

  return {
    policyId: nestedString(raw, "policyId"),
    strategy: nestedString(raw, "strategy"),
    candidatesConsidered: candidates,
    attempts: typeof attemptsRaw === "number" ? attemptsRaw : 0,
    selectedBy: nestedString(raw, "selectedBy"),
    reroutedFrom: nestedString(raw, "reroutedFrom"),
    requireLabels: labelsAt("requireLabels"),
    preferLabels: labelsAt("preferLabels"),
    present: raw !== null && Object.keys(raw).length > 0,
  };
}

export function invocationFromRow(row: Row): Invocation {
  return {
    id: rowString(row, "id"),
    createdAt: rowString(row, "createdAt"),
    tool: rowString(row, "tool"),
    action: rowString(row, "action"),
    outcome: rowString(row, "outcome"),
    durationMs: rowNumber(row, "durationMs"),
    errorCode: rowString(row, "errorCode"),
    errorMessage: rowString(row, "errorMessage"),
    routing: routingFromRow(row),
  };
}

// ---------------------------------------------------------------------------
// A workbench workspace
// ---------------------------------------------------------------------------

export interface Workspace {
  id: string;
  planId: string;
  ownerUserId: string;
  // MEMQL_NODE_ID of the workbench replica whose disk holds the directory.
  nodeId: string;
  status: string;
  storageRoot: string;
  createdAt: string;
  lastUsedAt: string;
  releasedAt: string;
  releasedReason: string;
}

export function workspaceFromRow(row: Row): Workspace {
  return {
    id: rowString(row, "id"),
    planId: rowString(row, "planId"),
    ownerUserId: rowString(row, "ownerUserId"),
    nodeId: rowString(row, "nodeId"),
    status: rowString(row, "status"),
    storageRoot: rowString(row, "storageRoot"),
    createdAt: rowString(row, "createdAt"),
    lastUsedAt: rowString(row, "lastUsedAt"),
    releasedAt: rowString(row, "releasedAt"),
    releasedReason: rowString(row, "releasedReason"),
  };
}

// What each release reason MEANS. node_lost is the one an operator has to be
// able to read off the page without going to the source: the files are gone
// with the replica and were not migrated, which is a deliberate design
// decision rather than a failure to recover them.
export const RELEASE_REASON_BLURB: Record<string, string> = {
  plan_terminal: "The plan finished, so its workspace was torn down.",
  explicit: "Released by hand from this page or a mutation.",
  ttl_expired: "Aged out by the idle sweep.",
  node_lost:
    "The workbench replica holding this directory left the mesh. The files went with it -- they are not migrated -- and the plan was given a fresh workspace elsewhere.",
};

// ---------------------------------------------------------------------------
// A workbench replica
// ---------------------------------------------------------------------------

export interface WorkbenchNode {
  id: string;
  nodeType: string;
  address: string;
  health: string;
  lastSeen: string;
  labels: LabelMap;
  capabilities: string[];
  region: string;
  provider: string;
  createdAt: string;
}

export function nodeFromRow(row: Row): WorkbenchNode {
  return {
    id: rowString(row, "id"),
    nodeType: rowString(row, "nodeType"),
    address: rowString(row, "address"),
    health: rowString(row, "health"),
    lastSeen: rowString(row, "lastSeen"),
    labels: labelMapFromRow(row, "labels"),
    capabilities: stringList(row, "capabilities"),
    region: rowString(row, "region"),
    provider: rowString(row, "provider"),
    createdAt: rowString(row, "createdAt"),
  };
}

// latestPerId collapses the append-only node stream to one row per id.
//
// v1:cluster:node is append-only -- every liveness transition writes a new row
// under the same id -- and `clusterNodes` declares no `asOf latest`, so it
// returns the WHOLE history. Its own DSL comment says the CLI collapses in Go;
// this is the same collapse, and without it a single replica renders once per
// heartbeat it has ever sent.
//
// Ties (two rows with the same createdAt, or none at all) keep the LAST one
// seen, which is the later row in the query's own order.
export function latestPerId(nodes: readonly WorkbenchNode[]): WorkbenchNode[] {
  const byId = new Map<string, WorkbenchNode>();
  for (const node of nodes) {
    const held = byId.get(node.id);
    if (held === undefined || node.createdAt >= held.createdAt) byId.set(node.id, node);
  }
  return [...byId.values()].sort((a, b) => a.id.localeCompare(b.id));
}
