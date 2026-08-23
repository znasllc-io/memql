import { useCallback, useEffect, useRef, useState } from "react";
import type { Event, Role, Row } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";
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
// TWO SUBSCRIPTIONS, ONE FOLD EACH
// ===========================================================================
// Workspaces and nodes are different concepts, so they are two subscriptions.
// Both are folded rather than used as refetch triggers, for the reason
// useMachines.ts spells out: a heartbeat-driven concept re-read on every event
// is a read every few seconds on an idle page.

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

function eventRow(event: Event): Row | null {
  if (event.payloadOmitted) return null;
  return event.payload ?? null;
}

function isDelete(event: Event): boolean {
  return event.kind.toLowerCase().includes("deleted");
}

export function useWorkbenches(): WorkbenchesState {
  const { query, subscriptions } = useCluster();
  const { access, loading: accessLoading } = useMyAccess();

  const [nodes, setNodes] = useState<WorkbenchNode[]>([]);
  const [nodesLoading, setNodesLoading] = useState(true);
  const [nodesError, setNodesError] = useState("");

  const [workspaces, setWorkspaces] = useState<Workspace[]>([]);
  const [workspacesLoading, setWorkspacesLoading] = useState(true);
  const [workspacesError, setWorkspacesError] = useState("");

  const [scope, setScope] = useState<WorkspaceScope>("mine");
  const [showReleased, setShowReleased] = useState(false);
  const [liveDegraded, setLiveDegraded] = useState("");
  const [epoch, setEpoch] = useState(0);
  const [busyId, setBusyId] = useState("");
  const [actionError, setActionError] = useState("");

  const userId = access?.userId ?? "";
  const role: Role = access?.clusterRole ?? "";
  const isClusterOwner = role === "owner";
  const accessResolved = !accessLoading && access !== null;
  const effectiveScope: WorkspaceScope = isClusterOwner ? scope : "mine";

  // The scope the workspaces currently on screen were read under. See the
  // workspaces effect for what it decides.
  const readScope = useRef<WorkspaceScope>(effectiveScope);

  // ---- the nodes --------------------------------------------------------
  useEffect(() => {
    if (query === null) return;
    let live = true;
    setNodesLoading(true);
    setNodesError("");

    void query
      .clusterNodes({})
      .then((result) => {
        if (!live) return;
        const all = result.rows().map(nodeFromRow);
        setNodes(latestPerId(all.filter((node) => node.nodeType === WORKBENCH_NODE_TYPE)));
      })
      .catch((err: unknown) => {
        if (live) setNodesError(describe(err));
      })
      .finally(() => {
        if (live) setNodesLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, epoch]);

  // ---- the workspaces ---------------------------------------------------
  useEffect(() => {
    if (query === null) return;
    let live = true;
    setWorkspacesLoading(true);
    setWorkspacesError("");
    // A SCOPE change clears the list; a reload or a toggle does not. Rows
    // belong to the scope they were read under, so holding another person's
    // workspaces across a switch renders them for a beat under the heading
    // "Your workspaces". The released toggle is exempt because it narrows the
    // SAME population, and the rows it keeps are the caller's own either way.
    if (readScope.current !== effectiveScope) {
      readScope.current = effectiveScope;
      setWorkspaces([]);
    }

    // The status narrowing is server-side for a cluster owner (allWorkspaces
    // declares the argument) and client-side for everyone else (myWorkspaces
    // does not). Same visible result; the difference is only how much comes
    // over the wire.
    const read =
      effectiveScope === "all"
        ? fleet.allWorkspaces(query, showReleased ? undefined : "provisioned")
        : fleet.myWorkspaces(query);

    void read
      .then((result) => {
        if (!live) return;
        const all = result.rows().map(workspaceFromRow);
        setWorkspaces(showReleased ? all : all.filter((one) => one.status !== "released"));
      })
      .catch((err: unknown) => {
        if (live) setWorkspacesError(describe(err));
      })
      .finally(() => {
        if (live) setWorkspacesLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, effectiveScope, showReleased, epoch]);

  // ---- the live folds ---------------------------------------------------
  const scopeRef = useRef(effectiveScope);
  scopeRef.current = effectiveScope;
  const userIdRef = useRef(userId);
  userIdRef.current = userId;
  const showReleasedRef = useRef(showReleased);
  showReleasedRef.current = showReleased;

  useEffect(() => {
    if (subscriptions === null) {
      setLiveDegraded("");
      return;
    }
    let live = true;
    const stops: (() => void)[] = [];

    try {
      stops.push(
        subscriptions.subscribeGraph(
          (event) => {
            if (!live) return;
            const row = eventRow(event);
            if (row === null) {
              setEpoch((n) => n + 1);
              return;
            }
            const workspace = workspaceFromRow(row);
            if (workspace.id === "") return;
            if (scopeRef.current === "mine" && workspace.ownerUserId !== userIdRef.current) return;

            setWorkspaces((current) => {
              const without = current.filter((held) => held.id !== workspace.id);
              // A release is an UPDATE, not a delete, and while released rows
              // are hidden the honest fold is to drop it from the list -- the
              // toggle is what says whether it belongs on screen, and leaving
              // it there would show a directory that no longer exists.
              if (isDelete(event)) return without;
              if (!showReleasedRef.current && workspace.status === "released") return without;
              return [workspace, ...without];
            });
          },
          {
            concept: WORKBENCH_WORKSPACE_CONCEPT_ID,
            actions: ["created", "updated", "deleted"],
          },
        ),
      );

      stops.push(
        subscriptions.subscribeGraph(
          (event) => {
            if (!live) return;
            const row = eventRow(event);
            if (row === null) {
              setEpoch((n) => n + 1);
              return;
            }
            const node = nodeFromRow(row);
            if (node.id === "" || node.nodeType !== WORKBENCH_NODE_TYPE) return;
            setNodes((current) =>
              isDelete(event)
                ? current.filter((held) => held.id !== node.id)
                : latestPerId([...current, node]),
            );
          },
          {
            concept: CLUSTER_NODE_CONCEPT_ID,
            actions: ["created", "updated", "deleted"],
          },
        ),
      );
      setLiveDegraded("");
    } catch (err) {
      setLiveDegraded(describe(err));
    }

    return () => {
      live = false;
      for (const stop of stops) stop();
    };
  }, [subscriptions]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);

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
