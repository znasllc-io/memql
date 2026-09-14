// THE READINESS FEED (design record 2026-09-06-configuration-readiness,
// section 5.2). Retained ONCE, in SessionScope, and provided through the
// session facts -- never re-read by a window or a Set up group, for the
// reason the machines feed gives: two subscriptions over one concept are free
// to disagree, and here the two readings would decide whether a setup surface
// or an app renders. A window showing the app while its Settings gear wears a
// "not set up" dot is one feed too many.
//
// Two collections: the readiness rows, and the cluster nodes whose liveness
// decides which rows count. Both carry broadcast routing rules
// (component/node/routing.go), so a change on any replica reaches the fold
// here -- without them the marks would be right on load and frozen after,
// which looks exactly like working.

import { useMemo } from "react";
import type { LiveState, Row } from "@znasllc-io/memql-sdk-core/client";

import type { ModuleId } from "../system/modules";
import {
  foldReadiness,
  type NodeLiveness,
  type NodeReport,
  type Verdict,
} from "../system/readinessFold";
import { useLiveCollection } from "./useLiveCollection";

export const MODULE_READINESS_CONCEPT = "v1:platform:moduleReadiness";
export const CLUSTER_NODE_CONCEPT = "v1:cluster:node";

export interface Readiness {
  /**
   * True once BOTH feeds have seeded. Nothing is gated or drawn before it,
   * so a configured cluster never flashes a setup screen for a frame -- and
   * any memo that filters by readiness must name `loaded` in its deps, the
   * rule the role ladder already enforces.
   */
  loaded: boolean;
  /** The worse of the two feeds' states, for a surface that wants to say "stale". */
  state: LiveState;
  of(id: ModuleId): Verdict | null;
  reseed(): void;
}

/**
 * One feed row to a report. A MISSING STATE IS "unknown", never
 * "unconfigured" (the 2026-09-14 readiness-convergence record, D1): the fold
 * sets unknown aside, while unconfigured is a vote that holds the core gate
 * and sends a person to a form -- the one direction a default must never take
 * over a row that said nothing. Exported for its own test; the feed is the
 * only production caller.
 */
export function reportFromRow(row: Row): NodeReport | null {
  const r = row as Record<string, unknown>;
  const module = typeof r.module === "string" ? r.module : "";
  if (module === "") return null;
  const reason = typeof r.reason === "string" && r.reason !== "" ? r.reason : undefined;
  return {
    module,
    nodeId: String(r.nodeId ?? ""),
    nodeType: String(r.nodeType ?? ""),
    state: String(r.state ?? "unknown") as NodeReport["state"],
    ...(reason !== undefined ? { reason } : {}),
    core: r.core === true,
    lanes: Array.isArray(r.lanes) ? (r.lanes as NodeReport["lanes"]) : [],
    reportedAt: String(r.reportedAt ?? ""),
  };
}

function livenessFromRow(row: Row): NodeLiveness {
  const r = row as Record<string, unknown>;
  const id = String(r.id ?? "").replace(/^v1:cluster:node:/, "");
  return {
    nodeId: id,
    health: String(r.health ?? "").toLowerCase(),
    lastSeen: String(r.lastSeen ?? ""),
  };
}

const LIVE_ORDER: LiveState[] = ["live", "seeding", "degraded", "disconnected"];
const worse = (a: LiveState, b: LiveState): LiveState =>
  LIVE_ORDER.indexOf(a) >= LIVE_ORDER.indexOf(b) ? a : b;

export function useReadinessFeed(): Readiness {
  const rows = useLiveCollection<Row>("readiness:rows", (connection) => ({
    concept: MODULE_READINESS_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.moduleReadinessAll({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    paged: false,
  }));
  const nodes = useLiveCollection<Row>("readiness:nodes", (connection) => ({
    concept: CLUSTER_NODE_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.staleClusterNodes({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    paged: false,
  }));

  const rowsVersion = rows.snapshot.version;
  const nodesVersion = nodes.snapshot.version;
  const loaded =
    rows.snapshot.state !== "seeding" &&
    nodes.snapshot.state !== "seeding" &&
    rows.source !== null &&
    nodes.source !== null;

  return useMemo(() => {
    // The snapshot object is replaced on every change, but the HANDLE is not,
    // so the version counters are what actually move -- naming them is what
    // makes this memo recompute.
    void rowsVersion;
    void nodesVersion;
    const reports = rows.snapshot.rows
      .map(reportFromRow)
      .filter((r): r is NodeReport => r !== null);
    const liveness = nodes.snapshot.rows.map(livenessFromRow);
    // `new Date()` at recompute time: a node that goes stale without any row
    // changing is re-judged on the next event, the same staleness the Fleet's
    // online dot accepts.
    const verdicts = loaded ? foldReadiness(reports, liveness, new Date()) : [];
    const byModule = new Map(verdicts.map((v) => [v.module, v]));
    return {
      loaded,
      state: worse(rows.snapshot.state, nodes.snapshot.state),
      of: (id: ModuleId) => byModule.get(id) ?? null,
      reseed: () => {
        rows.reseed();
        nodes.reseed();
      },
    };
  }, [loaded, rowsVersion, nodesVersion, rows, nodes]);
}
