import { rowArray, rowNumber, rowObject, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { labelMapFrom, mergeLabels, type LabelMap, type MergedLabel } from "./labels";

// The wire rows the Fleet renders, projected into the shapes its surfaces
// read.
//
// PURE, and separate from every component, for the reason the portal's
// src/fleet/rows.ts is: a projection asserted through render() is asserted
// through three layers that can each fail for unrelated reasons. Everything
// here is a function of a row and is unit-testable with no browser, no
// cluster and no React.
//
// ===========================================================================
// WHY EVERY PROJECTION FLATTENS FIRST
// ===========================================================================
// A row reaches these functions from two places: the SEED (a named query's
// result, already shape-flattened) and the SUBSCRIPTION fold (a CDC envelope).
// The envelope flattens the concept fields alongside the intrinsics, but a
// `payload`-wrapped form is what a raw graph event carries, and the two paths
// have to produce the same object or a machine would render one way on load
// and another way the moment its heartbeat lands. `flatten` is that one
// reconciliation, applied before any field is read.

/** Unwrap a `payload`-nested row to the flat form the field helpers read. */
export function flatten(row: Row): Row {
  const nested = row["payload"];
  if (nested && typeof nested === "object" && !Array.isArray(nested)) {
    // The intrinsics live on the envelope, the concept fields inside it;
    // the envelope wins on a collision so `id` stays the row's own id.
    return { ...(nested as Row), ...row };
  }
  return row;
}

function stringList(row: Row, key: string): string[] {
  const raw = rowArray(row, key) ?? [];
  return raw.filter((entry): entry is string => typeof entry === "string");
}

function numberMap(row: Row, key: string): Record<string, number> {
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

export interface MachineRow {
  id: string;
  ownerUserId: string;
  /** What the cockpit reported -- a hostname by default, and RE-STAMPED on
   *  every reconnect, which is why it is not the name a surface shows. */
  name: string;
  /** What the owner called it (renameWorker). Empty until renamed. */
  displayName: string;
  identityId: string;
  capabilities: string[];
  os: string;
  arch: string;
  hostname: string;
  platform: string;
  /** quartz / x11 / wayland / none, from the capability descriptor. Empty
   *  when the machine's build predates the descriptor -- a fact worth
   *  rendering as "not reported" rather than as "none". */
  displayServer: string;
  computerUseAvailable: boolean;
  reportedLabels: LabelMap;
  operatorLabels: LabelMap;
  mergedLabels: MergedLabel[];
  /** Per-capability parallelism cap, e.g. {HEADLESS: 8, COMPUTERUSE: 1}. */
  concurrency: Record<string, number>;
  /** Calls in flight as of the most recent heartbeat. Up to one interval
   *  stale by construction -- a routing input, never a correctness one. */
  activeCount: number;
  lastSeenAt: string;
  /** MEMQL_NODE_ID of the replica holding this machine's stream. Empty
   *  means no replica holds it. */
  connectedNodeId: string;
  lastSelectedAt: string;
  version: string;
  buildTag: string;
  registeredAt: string;
  revokedAt: string;
  revokedBy: string;
  revokeReason: string;
  /** The local apps this machine reported (memql#4359). */
  apps: MachineApp[];
}

/** One local app on a machine. */
export interface MachineApp {
  id: string;
  label: string;
  version: string;
  signedIn: boolean;
  allowed: boolean;
  /** unknown | none | present, as the app REPORTS it. Never inferred. */
  subscription: string;
  runnable: boolean;
  /** Empty when runnable; otherwise which half is missing, so a person
   *  reading the panel knows what to go and fix. */
  why: string;
}

// The engine's CLOSED runnable set (component/planner's executor registry and
// integrations/agent/worker/cockpitapp.go), mirrored so this surface agrees
// with selection. An id outside it is DISPLAYED -- the machine really has it
// -- and never marked runnable, because this engine has no protocol for it.
const RUNNABLE_APP_IDS = new Set(["claude-code", "codex"]);

const APP_LABELS: Record<string, string> = {
  "claude-code": "Claude Code",
  codex: "Codex",
};

export function appLabel(appId: string): string {
  return APP_LABELS[appId] ?? appId;
}

function appsFrom(row: Row): MachineApp[] {
  const raw = rowArray(row, "apps");
  if (raw === null) return [];
  return raw
    .map((item): MachineApp | null => {
      if (typeof item !== "object" || item === null || Array.isArray(item)) return null;
      const entry = item as Record<string, unknown>;
      const id = typeof entry["id"] === "string" ? entry["id"] : "";
      if (id === "") return null;
      const allowed = entry["allowed"] === true;
      const signedIn = entry["signedIn"] === true;
      const known = RUNNABLE_APP_IDS.has(id);
      const runnable = known && allowed && signedIn;
      return {
        id,
        label: appLabel(id),
        version: typeof entry["version"] === "string" ? entry["version"] : "",
        signedIn,
        allowed,
        subscription: typeof entry["subscription"] === "string" ? entry["subscription"] : "unknown",
        runnable,
        why: runnable
          ? ""
          : !known
            ? "this engine does not drive it"
            : !allowed
              ? "not in the machine's apps.allow"
              : "not signed in",
      };
    })
    .filter((app): app is MachineApp => app !== null)
    .sort((a, b) => a.id.localeCompare(b.id));
}

export function machineFromRow(raw: Row): MachineRow {
  const row = flatten(raw);
  const platformInfo = rowObject(row, "platformInfo");
  const descriptor = rowObject(row, "capabilityDescriptor");
  const reportedLabels = labelMapFrom(row["labels"]);
  const operatorLabels = labelMapFrom(row["operatorLabels"]);
  // platformInfo is the register-time snapshot; the descriptor repeats the
  // platform for machines that send one. platformInfo is preferred because
  // every machine has it.
  const os = nestedString(platformInfo, "os") || nestedString(descriptor, "platform");
  const arch = nestedString(platformInfo, "arch");

  return {
    id: rowString(row, "id"),
    ownerUserId: rowString(row, "ownerUserId"),
    name: rowString(row, "name"),
    displayName: rowString(row, "displayName"),
    identityId: rowString(row, "identityId"),
    capabilities: stringList(row, "capabilities"),
    os,
    arch,
    hostname: nestedString(platformInfo, "hostname"),
    platform: arch === "" ? os : `${os}/${arch}`,
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
    revokedBy: rowString(row, "revokedBy"),
    revokeReason: rowString(row, "revokeReason"),
    apps: appsFrom(row),
  };
}

/**
 * The name to render: `displayName` falling back to the reported `name`,
 * falling back to the id. ONE derivation, so the list, the detail panel and
 * the revoke confirmation cannot disagree about what a machine is called.
 */
export function machineName(m: Pick<MachineRow, "displayName" | "name" | "id">): string {
  return m.displayName.trim() || m.name.trim() || m.id;
}

/** A machine is revoked once `revokedAt` is written; the row survives as
 *  audit history, so "revoked" is a state rather than a deletion. */
export function isRevoked(m: Pick<MachineRow, "revokedAt">): boolean {
  return m.revokedAt.trim() !== "";
}

// ---------------------------------------------------------------------------
// The routing policy
// ---------------------------------------------------------------------------

/** The closed strategy set, in the order the editor offers them: the
 *  pre-policy default first, then the three that need a reason. */
export const ROUTING_STRATEGIES = ["firstFit", "roundRobin", "leastLoaded", "labelMatch"] as const;
export type RoutingStrategy = (typeof ROUTING_STRATEGIES)[number];

export const ROUTING_FALLBACKS = ["none", "nextMatching"] as const;
export type RoutingFallback = (typeof ROUTING_FALLBACKS)[number];

/** What the router does with no policy row at all. Named rather than
 *  written twice, because the "no policy" caption and the draft an editor
 *  opens with have to agree or the editor's first save would change
 *  behaviour the caption said was already in force. */
export const DEFAULT_STRATEGY: RoutingStrategy = "firstFit";
export const DEFAULT_FALLBACK: RoutingFallback = "nextMatching";

// What each value MEANS, in an operator's terms rather than the schema's.
// Rendered beside the control, because "leastLoaded" does not say what it is
// least-loaded against.
export const STRATEGY_BLURB: Record<RoutingStrategy, string> = {
  firstFit: "Registration order. What the router did before policies existed.",
  roundRobin:
    "Longest since last chosen first, so two replicas rotate the same way with no shared counter.",
  leastLoaded: "Fewest calls in flight first, against each capability's own cap.",
  labelMatch: "Most preferred labels matched first, then registration order.",
};

export const FALLBACK_BLURB: Record<RoutingFallback, string> = {
  none: "Report the refusal.",
  nextMatching:
    "Try the next candidate. Only ever before a call has started -- never a re-run.",
};

export interface RoutingPolicyRow {
  id: string;
  ownerUserId: string;
  strategy: string;
  requireLabels: LabelMap;
  preferLabels: LabelMap;
  fallback: string;
  active: boolean;
  createdAt: string;
}

export function routingPolicyFromRow(raw: Row): RoutingPolicyRow {
  const row = flatten(raw);
  return {
    id: rowString(row, "id"),
    ownerUserId: rowString(row, "ownerUserId"),
    strategy: rowString(row, "strategy"),
    requireLabels: labelMapFrom(row["requireLabels"]),
    preferLabels: labelMapFrom(row["preferLabels"]),
    fallback: rowString(row, "fallback"),
    active: row["active"] === true,
    createdAt: rowString(row, "createdAt"),
  };
}

/**
 * The policy the editor edits: the newest ACTIVE row, or nothing.
 *
 * myRoutingPolicies already sorts newest first, so "the first active row" is
 * the same choice routingPolicyForOwner makes server-side. That agreement is
 * the point -- an editor that picked a different row from the one the router
 * reads would write its edits somewhere nothing dispatches through.
 */
export function activePolicy(policies: readonly RoutingPolicyRow[]): RoutingPolicyRow | null {
  return policies.find((policy) => policy.active) ?? null;
}

// ---------------------------------------------------------------------------
// An invocation, and the routing record on it
// ---------------------------------------------------------------------------

export interface RoutingRecord {
  policyId: string;
  strategy: string;
  /** Registration ids the router filtered down to, in the order it would
   *  try them. */
  candidatesConsidered: string[];
  attempts: number;
  selectedBy: string;
  /** "workbench", or "worker:<registrationId>". Empty when nothing was
   *  rerouted. */
  reroutedFrom: string;
  requireLabels: LabelMap;
  preferLabels: LabelMap;
  /**
   * False for a row written before the router existed, and for a path that
   * never picked (a denial before the choice). Rendered as "no routing
   * decision recorded" rather than as an empty routing table, which would
   * read as "chose nothing" -- a different and wrong claim.
   */
  present: boolean;
}

export interface InvocationRow {
  id: string;
  createdAt: string;
  startedAt: string;
  tool: string;
  action: string;
  outcome: string;
  durationMs: number;
  errorCode: string;
  errorMessage: string;
  routing: RoutingRecord;
}

function routingFrom(row: Row): RoutingRecord {
  const raw = rowObject(row, "routing");
  const candidatesRaw = raw?.["candidatesConsidered"];
  const candidates = Array.isArray(candidatesRaw)
    ? candidatesRaw.filter((entry): entry is string => typeof entry === "string")
    : [];
  const attempts = raw?.["attempts"];
  return {
    policyId: nestedString(raw, "policyId"),
    strategy: nestedString(raw, "strategy"),
    candidatesConsidered: candidates,
    attempts: typeof attempts === "number" ? attempts : 0,
    selectedBy: nestedString(raw, "selectedBy"),
    reroutedFrom: nestedString(raw, "reroutedFrom"),
    requireLabels: labelMapFrom(raw?.["requireLabels"]),
    preferLabels: labelMapFrom(raw?.["preferLabels"]),
    present: raw !== null && Object.keys(raw).length > 0,
  };
}

export function invocationFromRow(raw: Row): InvocationRow {
  const row = flatten(raw);
  return {
    id: rowString(row, "id"),
    createdAt: rowString(row, "createdAt"),
    startedAt: rowString(row, "startedAt"),
    tool: rowString(row, "tool"),
    action: rowString(row, "action"),
    outcome: rowString(row, "outcome"),
    durationMs: rowNumber(row, "durationMs"),
    errorCode: rowString(row, "errorCode"),
    errorMessage: rowString(row, "errorMessage"),
    routing: routingFrom(row),
  };
}

/** Outcomes that are not a plain success, so a call reads as what it was.
 *  `rerouted` is deliberately here: the call ran, but not where the router
 *  first sent it, and that is the fact this surface exists to expose. */
export const OUTCOME_TONE: Record<string, "ok" | "warn" | "error"> = {
  success: "ok",
  rerouted: "warn",
  cancelled: "warn",
  timeout: "error",
  failure: "error",
  denied_by_scope: "error",
  denied_by_policy: "error",
  denied_by_classifier: "error",
  kill_switch_engaged: "error",
  no_worker_available: "error",
};

// ---------------------------------------------------------------------------
// A workbench workspace
// ---------------------------------------------------------------------------

export interface WorkspaceRow {
  id: string;
  planId: string;
  ownerUserId: string;
  /** MEMQL_NODE_ID of the workbench replica whose disk holds the directory. */
  nodeId: string;
  status: string;
  storageRoot: string;
  createdAt: string;
  lastUsedAt: string;
  releasedAt: string;
  releasedReason: string;
}

export function workspaceFromRow(raw: Row): WorkspaceRow {
  const row = flatten(raw);
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

/**
 * What each release reason MEANS. `node_lost` is the one an operator has to
 * be able to read off the screen without going to the source: the files are
 * gone with the replica and were NOT migrated, which is a deliberate design
 * decision (memql#4354) rather than a failure to recover them.
 */
export const RELEASE_REASON_BLURB: Record<string, string> = {
  plan_terminal: "The plan finished, so its workspace was torn down.",
  explicit: "Released by hand, from a fleet surface or a mutation.",
  ttl_expired: "Aged out by the idle sweep.",
  node_lost:
    "The workbench replica holding this directory left the mesh. The files went with it -- they are not migrated -- and the plan was given a fresh workspace elsewhere.",
};

// ---------------------------------------------------------------------------
// A workbench replica
// ---------------------------------------------------------------------------

export interface WorkbenchNodeRow {
  id: string;
  nodeType: string;
  address: string;
  health: string;
  lastSeen: string;
  createdAt: string;
}

export function nodeFromRow(raw: Row): WorkbenchNodeRow {
  const row = flatten(raw);
  return {
    id: rowString(row, "id"),
    nodeType: rowString(row, "nodeType"),
    address: rowString(row, "address"),
    health: rowString(row, "health"),
    lastSeen: rowString(row, "lastSeen"),
    createdAt: rowString(row, "createdAt"),
  };
}

/** The nodeType value that makes a v1:cluster:node row a workbench replica. */
export const WORKBENCH_NODE_TYPE = "workbench";

/**
 * Collapse the append-only node stream to one row per id.
 *
 * v1:cluster:node is append-only -- every liveness transition writes a new
 * row under the same id -- and `clusterNodes` declares no `asOf latest`, so
 * it returns the WHOLE history. Its own DSL comment says the CLI collapses in
 * Go; this is that same collapse, and without it a single replica renders
 * once per heartbeat it has ever sent.
 *
 * Ties (equal createdAt, or none at all) keep the LAST one seen, which is the
 * later row in the query's own order.
 */
export function latestPerId(nodes: readonly WorkbenchNodeRow[]): WorkbenchNodeRow[] {
  const byId = new Map<string, WorkbenchNodeRow>();
  for (const node of nodes) {
    const held = byId.get(node.id);
    if (held === undefined || node.createdAt >= held.createdAt) byId.set(node.id, node);
  }
  return [...byId.values()].sort((a, b) => a.id.localeCompare(b.id));
}

/**
 * Group workspaces by the replica whose disk holds them, newest first within
 * each group.
 *
 * A workspace with no `nodeId` is its own group under "" rather than being
 * dropped: a row written before memql#4354 stamped the field is still a
 * directory somewhere, and hiding it would answer "where did my files go"
 * with silence.
 */
export function workspacesByNode(
  workspaces: readonly WorkspaceRow[],
): Array<{ nodeId: string; workspaces: WorkspaceRow[] }> {
  const byNode = new Map<string, WorkspaceRow[]>();
  for (const one of workspaces) {
    const held = byNode.get(one.nodeId);
    if (held) held.push(one);
    else byNode.set(one.nodeId, [one]);
  }
  return [...byNode.entries()]
    .map(([nodeId, rows]) => ({
      nodeId,
      workspaces: [...rows].sort((a, b) =>
        a.createdAt === b.createdAt ? (a.id < b.id ? 1 : -1) : a.createdAt < b.createdAt ? 1 : -1,
      ),
    }))
    .sort((a, b) => a.nodeId.localeCompare(b.nodeId));
}
