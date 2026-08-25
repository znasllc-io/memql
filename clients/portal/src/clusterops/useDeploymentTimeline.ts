import { useCallback, useMemo } from "react";
import {
  browseConceptPage,
  getRowByConceptAndId,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";
import { useLive } from "../cluster/useLive";

// The deployment timeline (memql#4193), read from the graph state that
// already records it: v1:cluster:deployment is an append-only status
// timeline per deploymentId, and v1:cluster:deploymentNodeSpec pins each
// node type inside one. The record IS the progress source -- after an
// action the page re-reads these rows rather than inventing a spinner
// state of its own.
//
// LIVE THROUGH THE STORE (memql#4539). A deployment row landing anywhere in
// the cluster used to re-read BOTH pages -- and a deploy writes a status row
// per transition, so watching one meant a 250-row read pair every few seconds.
// Both concepts ride collections now and fold; the join and the per-id
// collapse below are unchanged, because they are this page's reading of an
// append-only history rather than anything about folding.
//
// THE COLLAPSE SORTS EXPLICITLY. It used to lean on the read's `desc` order
// and take the first row seen per id -- correct for a page, wrong for a store,
// where a folded arrival lands after everything the seed returned. Sorting on
// createdAt says what was actually meant and is right under both.

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
  const { query, status } = useCluster();
  const connected = enabled && query !== null && status === "connected";

  const deploymentsLive = useLive<Row>(
    connected ? "clusterops:deployments" : null,
    () => ({
      concept: DEPLOYMENT_CONCEPT,
      actions: ["created", "updated"],
      paged: false,
      seed: async (_cursor, signal) => {
        if (query === null) return { rows: [], nextCursor: "" };
        const page = await browseConceptPage(query, DEPLOYMENT_CONCEPT, {
          pageSize: TIMELINE_PAGE,
          order: "desc",
          signal,
        });
        return { rows: page.rows, nextCursor: "" };
      },
      reread: async (rowId, signal) => {
        if (query === null) return null;
        return getRowByConceptAndId(query, DEPLOYMENT_CONCEPT, rowId, { signal });
      },
    }),
  );

  const specsLive = useLive<Row>(connected ? "clusterops:deploymentNodeSpecs" : null, () => ({
    concept: NODE_SPEC_CONCEPT,
    actions: ["created", "updated"],
    paged: false,
    seed: async (_cursor, signal) => {
      if (query === null) return { rows: [], nextCursor: "" };
      const page = await browseConceptPage(query, NODE_SPEC_CONCEPT, {
        pageSize: 200,
        order: "desc",
        signal,
      });
      return { rows: page.rows, nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      if (query === null) return null;
      return getRowByConceptAndId(query, NODE_SPEC_CONCEPT, rowId, { signal });
    },
  }));

  const entries = useMemo<DeploymentEntry[]>(() => {
    const specsByDeployment = new Map<
      string,
      Map<string, { nodeType: string; version: string; replicas: number }>
    >();
    for (const row of newestFirst(specsLive.rows)) {
      const p = payloadOf(row);
      const deploymentId = str(p["deploymentId"]);
      const nodeType = str(p["nodeType"]);
      if (deploymentId === "" || nodeType === "") continue;
      const byType = specsByDeployment.get(deploymentId) ?? new Map();
      // Newest-first walk: the first spec row seen per (deployment, nodeType)
      // is the current pin; older re-pins are its history.
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
    for (const row of newestFirst(deploymentsLive.rows)) {
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
    return collapsed;
  }, [deploymentsLive.rows, specsLive.rows]);

  const reload = useCallback(() => {
    deploymentsLive.reload();
    specsLive.reload();
  }, [deploymentsLive, specsLive]);

  return {
    entries: connected ? entries : [],
    loading: connected && (deploymentsLive.state === "seeding" || specsLive.state === "seeding"),
    error: deploymentsLive.error || specsLive.error,
    reload,
  };
}

// newestFirst orders an append-only concept's rows by createdAt descending,
// ties broken on id so the order is total and does not reshuffle between
// renders. The collapse above depends on it: taking "the first row seen" is
// only the current one if the walk is actually newest-first.
function newestFirst(rows: readonly Row[]): Row[] {
  return [...rows].sort((a, b) => {
    const at = str((a as { createdAt?: unknown }).createdAt);
    const bt = str((b as { createdAt?: unknown }).createdAt);
    if (at !== bt) return at < bt ? 1 : -1;
    const ai = str((a as { id?: unknown }).id);
    const bi = str((b as { id?: unknown }).id);
    return ai === bi ? 0 : ai < bi ? 1 : -1;
  });
}
