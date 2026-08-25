import { useCallback, useMemo, useState } from "react";
import { getRowByConceptAndId, type Role, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";
import { useLive } from "../cluster/useLive";
import { useMyAccess } from "../cluster/useMyAccess";
import * as fleet from "./calls";
import {
  CLUSTER_NODE_CONCEPT_ID,
  WORKBENCH_NODE_TYPE,
  WORKBENCH_WORKSPACE_CONCEPT_ID,
} from "./concepts";
import {
  latestPerId,
  nodeFromRow,
  workspaceFromRow,
  type WorkbenchNode,
  type Workspace,
} from "./rows";

// The workbenches screen's state: the replicas that host workspaces, and the
// workspaces living on them.
//
// ===========================================================================
// THE NODE READ IS clusterNodes, AND IT IS NOT NARROWED SERVER-SIDE
// ===========================================================================
// dsl/cluster/queries.memql declares no per-nodeType read: clusterNodes takes
// no arguments and nodesForDeployment / nodesNotInDeployment narrow by
// deployment, not by role. Adding one would be a DSL change, which this
// surface does not own -- so the narrowing to nodeType=workbench happens here,
// after the read.
//
// clusterNodes ALSO returns the whole append-only history (its own comment
// says the CLI collapses to latest-per-id in Go, because `asOf latest` is a
// query-level directive the loader rejects there). rows.latestPerId is that
// same collapse; without it one replica renders once per liveness row it has
// ever written.
//
// staleClusterNodes would have collapsed server-side -- it declares
// `asOf latest` -- but it also filters `health!="stopped"`, so a replica that
// stopped would vanish from a page whose whole job is saying where a
// workspace's files went. A node that stopped is the most interesting row on
// this screen, not one to hide.
//
// ===========================================================================
// TWO COLLECTIONS, ONE PER CONCEPT
// ===========================================================================
// Workspaces and nodes are different concepts, so they are two subscriptions
// and two collections. Both FOLD rather than re-read on every event, for the
// reason useMachines.ts spells out: a heartbeat-driven concept re-read per
// event is a read every few seconds on an idle page.
//
// The folds themselves are the SDK's since memql#4539 -- this file used to
// carry two hand-rolled splices, both deciding "is this a delete" by
// lowercasing the event kind and looking for a substring. The node collapse
// (latestPerId) and the released-workspace narrowing stay here, because they
// are this page's reading of the rows rather than anything about folding.

export type WorkspaceScope = "mine" | "all";

export interface WorkbenchesState {
  nodes: WorkbenchNode[];
  nodesLoading: boolean;
  nodesError: string;

  workspaces: Workspace[];
  workspacesLoading: boolean;
  workspacesError: string;

  scope: WorkspaceScope;
  setScope: (scope: WorkspaceScope) => void;
  // Whether released rows are listed. Off by default: a released workspace is
  // a directory that no longer exists, and the standing question this page
  // answers is which ones DO.
  showReleased: boolean;
  setShowReleased: (show: boolean) => void;

  isClusterOwner: boolean;
  accessResolved: boolean;
  role: Role;
  userId: string;

  liveDegraded: string;
  reload: () => void;

  busyId: string;
  actionError: string;
  release: (workspaceId: string) => void;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// liveNotice turns a collection's state into the page's "live updates are
// off" sentence, or "" when it is live. Kept separate from the read error for
// useConceptRows' reason: a successful read must not erase the notice, or the
// list looks live moments after going deaf.
function liveNotice(state: string): string {
  if (state === "disconnected") {
    return "the connection to the cluster dropped -- these rows are as of the last update";
  }
  if (state === "degraded") return "live updates were interrupted -- refreshing";
  return "";
}

export function useWorkbenches(): WorkbenchesState {
  const { query } = useCluster();
  const { access, loading: accessLoading } = useMyAccess();

  const [scope, setScope] = useState<WorkspaceScope>("mine");
  const [showReleased, setShowReleased] = useState(false);
  const [busyId, setBusyId] = useState("");
  const [actionError, setActionError] = useState("");

  const userId = access?.userId ?? "";
  const role: Role = access?.clusterRole ?? "";
  const isClusterOwner = role === "owner";
  const accessResolved = !accessLoading && access !== null;
  const effectiveScope: WorkspaceScope = isClusterOwner ? scope : "mine";

  // ---- the nodes --------------------------------------------------------
  const nodesLive = useLive<Row>(query === null ? null : "fleet:workbench:nodes", () => ({
    concept: CLUSTER_NODE_CONCEPT_ID,
    actions: ["created", "updated", "deleted"],
    paged: false,
    seed: async (_cursor, signal) => {
      if (query === null) return { rows: [], nextCursor: "" };
      const result = await query.clusterNodes({}, { signal });
      // The nodeType narrowing is CLIENT-side because dsl/cluster declares no
      // per-nodeType read (see the header) -- so it belongs here, in the seed,
      // which is where this read's meaning is written. `inScope` says the same
      // thing about arriving events.
      return {
        rows: result.rows().filter((row) => nodeFromRow(row).nodeType === WORKBENCH_NODE_TYPE),
        nextCursor: "",
      };
    },
    reread: async (rowId, signal) => {
      if (query === null) return null;
      return getRowByConceptAndId(query, CLUSTER_NODE_CONCEPT_ID, rowId, { signal });
    },
    inScope: (row) => nodeFromRow(row).nodeType === WORKBENCH_NODE_TYPE,
  }));

  // latestPerId is the append-only collapse the header describes. It runs
  // over the collection's rows rather than inside it, because it is this
  // page's reading of a history -- the store's job is which rows exist, not
  // which version of one to show.
  const nodes = useMemo(
    () =>
      latestPerId(
        nodesLive.rows
          .map(nodeFromRow)
          .filter((node) => node.id !== "" && node.nodeType === WORKBENCH_NODE_TYPE),
      ),
    [nodesLive.rows],
  );

  // ---- the workspaces ---------------------------------------------------
  //
  // The scope is part of the KEY, so switching is a different collection
  // rather than a re-read of this one: rows belong to the scope they were read
  // under, and sharing one across the toggle would render another person's
  // workspaces for a beat under the heading "Your workspaces".
  //
  // The RELEASED toggle is in the key too, because for a cluster owner it
  // changes the READ: allWorkspaces declares a status argument and narrows
  // server-side, while myWorkspaces does not and narrows below. Keeping the
  // toggle out of the key would quietly turn the owner's narrowing into a
  // client-side filter over the whole cluster's history. Toggling back reuses
  // the first collection.
  const workspacesLive = useLive<Row>(
    query === null ? null : `fleet:workspaces:${effectiveScope}:${userId}:${showReleased}`,
    () => ({
      concept: WORKBENCH_WORKSPACE_CONCEPT_ID,
      actions: ["created", "updated", "deleted"],
      paged: false,
      seed: async (_cursor, signal) => {
        if (query === null) return { rows: [], nextCursor: "" };
        // Owner reads narrow server-side (allWorkspaces declares the argument);
        // everyone else narrows in `workspaces` below. Same visible result --
        // the difference is only how much comes over the wire.
        const result =
          effectiveScope === "all"
            ? showReleased
              ? await fleet.allWorkspaces(query, { signal })
              : await fleet.allWorkspaces(query, "provisioned", { signal })
            : await fleet.myWorkspaces(query, { signal });
        return { rows: result.rows(), nextCursor: "" };
      },
      reread: async (rowId, signal) => {
        if (query === null) return null;
        return getRowByConceptAndId(query, WORKBENCH_WORKSPACE_CONCEPT_ID, rowId, { signal });
      },
      inScope: (row) =>
        effectiveScope !== "mine" || workspaceFromRow(row).ownerUserId === userId,
    }),
  );

  const workspaces = useMemo(() => {
    const all = workspacesLive.rows.map(workspaceFromRow).filter((one) => one.id !== "");
    // A release is an UPDATE, not a delete, so a released workspace stays in
    // the collection and the toggle decides whether it belongs on screen --
    // rendering one while the toggle is off would show a directory that no
    // longer exists.
    const visible = showReleased ? all : all.filter((one) => one.status !== "released");
    // Newest first, which is what the splice this replaced produced by
    // prepending. The collection preserves ARRIVAL order, which is the read's
    // order plus late arrivals at the end -- fine for a set, wrong for a list
    // an operator scans top-down for "what just happened".
    return [...visible].sort((a, b) =>
      a.createdAt === b.createdAt ? (a.id < b.id ? 1 : -1) : a.createdAt < b.createdAt ? 1 : -1,
    );
  }, [workspacesLive.rows, showReleased]);

  const nodesLoading = query === null || nodesLive.state === "seeding";
  const workspacesLoading = query === null || workspacesLive.state === "seeding";
  const nodesError = nodesLive.error;
  const workspacesError = workspacesLive.error;
  // One notice for the page: either feed being behind means the screen is.
  const liveDegraded = liveNotice(nodesLive.state) || liveNotice(workspacesLive.state);

  const reload = useCallback(() => {
    nodesLive.reload();
    workspacesLive.reload();
  }, [nodesLive, workspacesLive]);

  const release = useCallback(
    (workspaceId: string) => {
      if (query === null) return;
      setBusyId(workspaceId);
      setActionError("");
      void query
        // releaseWorkspace predates this epic, so it is on the generated typed
        // surface and is called like every other write in the portal.
        //
        // "explicit" is the reason, and it is the honest one: a person pressed
        // a button. plan_terminal belongs to the automation, ttl_expired to a
        // sweep, and node_lost to the mesh noticing a replica has gone.
        .releaseWorkspace({ workspaceId, reason: "explicit" })
        .catch((err: unknown) => setActionError(describe(err)))
        .finally(() => setBusyId(""));
    },
    [query],
  );

  return {
    nodes,
    nodesLoading,
    nodesError,
    workspaces,
    workspacesLoading,
    workspacesError,
    scope: effectiveScope,
    setScope,
    showReleased,
    setShowReleased,
    isClusterOwner,
    accessResolved,
    role,
    userId,
    liveDegraded,
    reload,
    busyId,
    actionError,
    release,
  };
}
