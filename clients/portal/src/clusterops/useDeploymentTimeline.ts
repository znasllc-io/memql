import { useCallback, useEffect, useState } from "react";
import { browseConceptPage, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";

// The deployment timeline (memql#4193), read from the graph state that
// already records it: v1:cluster:deployment is an append-only status
// timeline per deploymentId, and v1:cluster:deploymentNodeSpec pins each
// node type inside one. The record IS the progress source -- after an
// action the page re-reads these rows rather than inventing a spinner
// state of its own.
//
// LIVE per the #4180 grammar where it is cheap: a graph subscription on the
// deployment concept triggers a refetch (one newest-first page), so a
// deploy started here -- or anywhere else -- ticks in without polling.

const DEPLOYMENT_CONCEPT = "v1:cluster:deployment";
const NODE_SPEC_CONCEPT = "v1:cluster:deploymentNodeSpec";
const TIMELINE_PAGE = 50;

// A repair (memql#4209) is a deployment record too -- same concept, same
// timeline -- marked by the engine with a "repair:" note prefix, since the
// concept carries no kind field. The prefix is the one convention a reader
// needs to tell a repair from a deploy; it is defined once, in
// component/deploycontrol/repair.go (repairNotePrefix), and mirrored here.
const REPAIR_NOTE_PREFIX = "repair:";

export type DeploymentKind = "deploy" | "repair";

export interface DeploymentEntry {
  rowId: string;
  deploymentId: string;
  status: string;
  engineVersion: string;
  createdAt: string;
  kind: DeploymentKind;
  notes: string;
  nodeSpecs: Array<{ nodeType: string; version: string; replicas: number }>;
}

export function deploymentKindOf(notes: string): DeploymentKind {
  return notes.trimStart().startsWith(REPAIR_NOTE_PREFIX) ? "repair" : "deploy";
}

export interface TimelineState {
  entries: DeploymentEntry[];
  loading: boolean;
  error: string;
  reload: () => void;
}

function payloadOf(row: Row): Record<string, unknown> {
  const p = (row as { payload?: unknown }).payload;
  return typeof p === "object" && p !== null
    ? (p as Record<string, unknown>)
    : (row as Record<string, unknown>);
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

function num(v: unknown): number {
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}

export function useDeploymentTimeline(enabled: boolean): TimelineState {
  const { query, subscriptions, status } = useCluster();
  const [entries, setEntries] = useState<DeploymentEntry[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (!enabled || !query || status !== "connected") return;
    let stale = false;
    setLoading(true);
    setError("");
    (async () => {
      // Newest deployment rows first. The concept is append-only per
      // deploymentId, so the newest row per id is that deployment's current
      // status; older rows of the same id are its history and collapse here.
      const deployments = await browseConceptPage(query, DEPLOYMENT_CONCEPT, {
        pageSize: TIMELINE_PAGE,
        order: "desc",
      });
      const specs = await browseConceptPage(query, NODE_SPEC_CONCEPT, {
        pageSize: 200,
        order: "desc",
      });

      const specsByDeployment = new Map<string, Map<string, { nodeType: string; version: string; replicas: number }>>();
      for (const row of specs.rows) {
        const p = payloadOf(row);
        const deploymentId = str(p["deploymentId"]);
        const nodeType = str(p["nodeType"]);
        if (deploymentId === "" || nodeType === "") continue;
        const byType = specsByDeployment.get(deploymentId) ?? new Map();
        // Desc walk: the first spec row seen per (deployment, nodeType) is
        // the current pin; older re-pins are history.
        if (!byType.has(nodeType)) {
          byType.set(nodeType, {
            nodeType,
            version: str(p["version"]),
            replicas: num(p["replicas"]),
          });
        }
        specsByDeployment.set(deploymentId, byType);
      }

      const seen = new Set<string>();
      const collapsed: DeploymentEntry[] = [];
      for (const row of deployments.rows) {
        const p = payloadOf(row);
        const deploymentId = str(p["deploymentId"]) || str((row as { id?: unknown }).id);
        if (deploymentId === "" || seen.has(deploymentId)) continue;
        seen.add(deploymentId);
        const notes = str(p["notes"]);
        collapsed.push({
          rowId: str((row as { id?: unknown }).id),
          deploymentId,
          status: str(p["status"]),
          engineVersion: str(p["engineVersion"]) || str(p["version"]),
          createdAt: str((row as { createdAt?: unknown }).createdAt),
          kind: deploymentKindOf(notes),
          notes,
          nodeSpecs: [...(specsByDeployment.get(deploymentId)?.values() ?? [])].sort((a, b) =>
            a.nodeType.localeCompare(b.nodeType),
          ),
        });
      }
      if (!stale) setEntries(collapsed);
    })()
      .catch((err: unknown) => {
        if (!stale) setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (!stale) setLoading(false);
      });
    return () => {
      stale = true;
    };
  }, [enabled, query, status, epoch]);

  // A deployment row landing anywhere in the cluster re-reads the page.
  useEffect(() => {
    if (!enabled || !subscriptions || status !== "connected") return;
    try {
      return subscriptions.subscribeGraph(() => setEpoch((n) => n + 1), {
        concept: DEPLOYMENT_CONCEPT,
        actions: ["created", "updated"],
      });
    } catch {
      // A failed subscription degrades to the manual Refresh -- the page
      // stays correct, just not live.
      return;
    }
  }, [enabled, subscriptions, status]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);
  return { entries, loading, error, reload };
}
