// The readiness fold, restated for the shell (design record
// 2026-09-06-configuration-readiness, section 4.5). component/memql/readiness is
// the source; this is the copy the live feed runs, and the shared fixtures
// under component/memql/readiness/testdata/fold hold the two equal -- a case added
// on either side is a case both must pass.
//
// THE LITERAL IS PARSED. component/memql/readiness/os_parity_test.go extracts
// NODE_LIVE_WINDOW_SECONDS by regexp and fails the build when it disagrees
// with NodeLiveWindow. Keep it a plain numeric literal.
export const NODE_LIVE_WINDOW_SECONDS = 60;

export type ReadinessState =
  | "configured"
  | "partial"
  | "unconfigured"
  | "notApplicable"
  | "unreported"
  /**
   * A node's word for "I could not evaluate this" (the 2026-09-14
   * readiness-convergence record, D1). Never a vote: the fold sets it aside
   * and names the node in `Verdict.unknown`.
   */
  | "unknown";

/**
 * The one lane scope there is. A lane marked `cluster` is computed from
 * cluster rows alone, so two nodes that disagree about it read the cluster at
 * different moments -- and the older one is set aside as stale rather than
 * folded (memql#5259). An unmarked lane is the node's own answer.
 */
export const LANE_SCOPE_CLUSTER = "cluster";

export interface SlotReport {
  name: string;
  present: boolean;
  source: string;
  optional?: boolean;
}

export interface LaneReport {
  name: string;
  configurableFrom: string;
  /** `cluster` for a lane computed from cluster rows alone; absent otherwise. */
  scope?: string;
  complete: boolean;
  slots: SlotReport[];
}

export interface NodeReport {
  module: string;
  nodeId: string;
  nodeType: string;
  state: ReadinessState;
  /** Set only when state is "unknown": fleetReadFailed | integrationProbeFailed. */
  reason?: string;
  core?: boolean;
  lanes?: LaneReport[];
  reportedAt: string;
}

export interface NodeLiveness {
  nodeId: string;
  health: string;
  lastSeen: string;
}

export interface NodeVerdict {
  nodeId: string;
  nodeType: string;
  state: ReadinessState;
  reportedAt: string;
}

/**
 * A live node whose report the fold SET ASIDE rather than counted, with what
 * its own row said and why it was set aside. Not part of the Go Verdict, which
 * names these nodes and no more; the operator's per-node reading in the
 * Cluster app needs the time and the reason to say "behind since 09:14" or
 * "the fleet read failed" rather than a bare id.
 */
export interface AsideVerdict {
  nodeId: string;
  nodeType: string;
  state: ReadinessState;
  reportedAt: string;
  reason?: string;
  why: "stale" | "unknown";
}

export interface Verdict {
  module: string;
  state: ReadinessState;
  core: boolean;
  disagreement: string[];
  nodes: NodeVerdict[];
  /**
   * Every LIVE node whose row says it could not evaluate, sorted. Those rows
   * are neither in nodes nor in disagreement; they cast no vote.
   */
  unknown: string[];
  /**
   * Every LIVE node whose row read the cluster before the freshest one did --
   * its cluster-scoped lanes disagree with the newest row's -- sorted. Set
   * aside until it re-evaluates, which its own safety net guarantees within
   * one period (memql#5259).
   */
  stale: string[];
  /** The stale and unknown nodes' own rows, for the per-node reading. TS only. */
  aside: AsideVerdict[];
  /**
   * The worst live reporter's lanes, for the Set up group; empty when
   * unreported. Not part of the Go Verdict, which the group does not read --
   * the shell needs the slot names to say what to set, and the engine's
   * consumer of the fold does not.
   */
  lanes: LaneReport[];
}

const LIVE_HEALTH = new Set(["healthy", "connecting", "degraded", "draining"]);

/**
 * Whether a node's report may count: a live health word and a heartbeat
 * inside the window. A missing or unparseable lastSeen is never live --
 * "we do not know when this node was last seen" is not evidence that it is up.
 */
export function nodeIsLive(n: NodeLiveness, now: Date): boolean {
  if (!LIVE_HEALTH.has(n.health)) return false;
  if (!n.lastSeen) return false;
  const seen = Date.parse(n.lastSeen);
  if (Number.isNaN(seen)) return false;
  return now.getTime() - seen <= NODE_LIVE_WINDOW_SECONDS * 1000;
}

function rank(s: ReadinessState): number {
  if (s === "unconfigured") return 2;
  if (s === "partial") return 1;
  return 0;
}

/**
 * Every node's report to one verdict per module:
 *
 *  1. Keep reports from live nodes only, so a dead replica's stale row cannot
 *     pin a verdict.
 *  2. Drop notApplicable.
 *  3. Set aside unknown: a node that could not evaluate is named in `unknown`
 *     and casts no vote.
 *  4. Set aside a row whose CLUSTER-SCOPED lanes disagree with the freshest
 *     row's: it read the cluster before something changed, and it is named in
 *     `stale` rather than folded (see setAsideStale).
 *  5. Worst state wins.
 *  6. If the kept reports disagree, the verdict is partial and disagreement
 *     names every kept reporter as nodeId=state, worst first.
 *  7. A module with no kept report is unreported -- never unconfigured, and
 *     that includes a module whose every live row is unknown. Not knowing and
 *     not being set up are different answers, and only one of them should send
 *     somebody to a form.
 *
 * Modules come back in name order, so two folds over the same rows are equal.
 */
export function foldReadiness(reports: NodeReport[], nodes: NodeLiveness[], now: Date): Verdict[] {
  const live = new Set(nodes.filter((n) => nodeIsLive(n, now)).map((n) => n.nodeId));
  const kept = new Map<string, NodeReport[]>();
  const unknown = new Map<string, NodeReport[]>();
  const core = new Map<string, boolean>();
  const order: string[] = [];
  for (const r of reports) {
    if (!core.has(r.module)) {
      order.push(r.module);
      core.set(r.module, false);
    }
    if (r.core) core.set(r.module, true);
    if (!live.has(r.nodeId) || r.state === "notApplicable") continue;
    const into = r.state === "unknown" ? unknown : kept;
    const list = into.get(r.module) ?? [];
    list.push(r);
    into.set(r.module, list);
  }
  order.sort();
  const out: Verdict[] = [];
  for (const module of order) {
    const unknownRows = [...(unknown.get(module) ?? [])].sort(byNodeId);
    const { voters: rs, stale: staleRows } = setAsideStale(kept.get(module) ?? []);
    const aside: AsideVerdict[] = [
      ...staleRows.map((r) => asideOf(r, "stale")),
      ...unknownRows.map((r) => asideOf(r, "unknown")),
    ];
    const base = {
      module,
      core: core.get(module) ?? false,
      unknown: unknownRows.map((r) => r.nodeId),
      stale: staleRows.map((r) => r.nodeId),
      aside,
    };
    if (rs.length === 0) {
      out.push({ ...base, state: "unreported", disagreement: [], nodes: [], lanes: [] });
      continue;
    }
    rs.sort((a, b) => rank(b.state) - rank(a.state) || byNodeId(a, b));
    const states = new Set(rs.map((r) => r.state));
    out.push({
      ...base,
      state: states.size > 1 ? "partial" : rs[0]!.state,
      disagreement: states.size > 1 ? rs.map((r) => `${r.nodeId}=${r.state}`) : [],
      nodes: rs.map((r) => ({
        nodeId: r.nodeId,
        nodeType: r.nodeType,
        state: r.state,
        reportedAt: r.reportedAt,
      })),
      lanes: rs[0]!.lanes ?? [],
    });
  }
  return out;
}

function byNodeId(a: { nodeId: string }, b: { nodeId: string }): number {
  return a.nodeId < b.nodeId ? -1 : a.nodeId > b.nodeId ? 1 : 0;
}

function asideOf(r: NodeReport, why: AsideVerdict["why"]): AsideVerdict {
  return {
    nodeId: r.nodeId,
    nodeType: r.nodeType,
    state: r.state,
    reportedAt: r.reportedAt,
    ...(r.reason ? { reason: r.reason } : {}),
    why,
  };
}

/**
 * The stale rule, exactly as component/memql/readiness states it: the
 * freshest report that carries cluster-scoped lanes is the reference, and a
 * report whose cluster-scoped completeness differs from it read the cluster at
 * an earlier moment. When the freshest moment holds reports that disagree
 * among themselves, recency cannot say which is right and NOTHING is set
 * aside. A report with no cluster-scoped lane -- every module but inference,
 * and every row written before lanes carried a scope -- is never judged.
 *
 * Times compare as instants. Rows carry RFC3339 to the second; a timestamp
 * that does not parse is never the freshest, and never reads as newer than
 * one that does.
 */
function setAsideStale(rs: NodeReport[]): { voters: NodeReport[]; stale: NodeReport[] } {
  let freshest = Number.NEGATIVE_INFINITY;
  let found = false;
  for (const r of rs) {
    if (clusterFacts(r) === null) continue;
    const at = instant(r.reportedAt);
    if (!found || at > freshest) {
      freshest = at;
      found = true;
    }
  }
  if (!found) return { voters: rs, stale: [] };
  let ref: Map<string, boolean> | null = null;
  for (const r of rs) {
    const facts = clusterFacts(r);
    if (facts === null || instant(r.reportedAt) !== freshest) continue;
    if (ref === null) {
      ref = facts;
      continue;
    }
    if (!sameFacts(ref, facts)) return { voters: rs, stale: [] };
  }
  const voters: NodeReport[] = [];
  const stale: NodeReport[] = [];
  for (const r of rs) {
    const facts = clusterFacts(r);
    if (facts !== null && ref !== null && !sameFacts(ref, facts)) {
      stale.push(r);
      continue;
    }
    voters.push(r);
  }
  stale.sort(byNodeId);
  return { voters, stale };
}

function instant(at: string): number {
  const t = Date.parse(at);
  return Number.isNaN(t) ? Number.NEGATIVE_INFINITY : t;
}

function clusterFacts(r: NodeReport): Map<string, boolean> | null {
  let facts: Map<string, boolean> | null = null;
  for (const lane of r.lanes ?? []) {
    if (lane.scope !== LANE_SCOPE_CLUSTER) continue;
    facts ??= new Map();
    facts.set(lane.name, lane.complete);
  }
  return facts;
}

/** Completeness only: a lane's `live` slot is presence, not a fact about the cluster. */
function sameFacts(a: Map<string, boolean>, b: Map<string, boolean>): boolean {
  if (a.size !== b.size) return false;
  for (const [name, complete] of a) {
    if (b.get(name) !== complete || !b.has(name)) return false;
  }
  return true;
}
