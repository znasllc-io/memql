import { useCallback, useEffect, useState } from "react";
import type { Role } from "@znasllc-io/memql-sdk-core/client";
import {
  DeployControlError,
  cutVersion,
  deploy as deployRelease,
  getDeploymentStatus,
  rollbackDeployment,
  type DeployEnv,
  type DeploymentStatus,
  type SemverBump,
} from "@znasllc-io/memql-sdk-core/deploy";

import { useCluster } from "../cluster/ClusterProvider";
import { useMyAccess } from "../cluster/useMyAccess";

// The deploy console, wired to the stream.
//
// memql#3311 bridged DeployControlService onto MemqlService.Stream and
// sdk/ts/src/deploy is the typed client over that bridge; this is its first
// consumer.
//
// ===========================================================================
// THE GATE IS THE SERVER'S. THIS IS THE UI'S READING OF IT.
// ===========================================================================
// The role matrix below is a COPY of a decision made in
// component/deploycontrol/service.go, and copies drift. It is here anyway,
// for one reason: a console that renders "Roll back" to an admin, lets them
// click it, and then shows PermissionDenied has wasted their time and taught
// them nothing about who can. Hiding an action they cannot take is the honest
// interface.
//
// What it is NOT is enforcement. Every function below issues the same RPC the
// unary path issues, the server applies the same matrix, and a caller below
// the floor gets PermissionDenied whatever this file believes -- which is
// exactly what would happen if someone edited the constants here. When the two
// disagree, the server wins and the console surfaces its message verbatim.
//
//   view (getDeploymentStatus)              admin / owner
//   cut + deploy                            developer / admin / owner
//   roll back                               OWNER ONLY -- not even admin
//
// See docs/public/operate/deployment-console.md.

const CAN_VIEW: readonly Role[] = ["owner", "admin"];
const CAN_SHIP: readonly Role[] = ["owner", "admin", "developer"];
const CAN_ROLL_BACK: readonly Role[] = ["owner"];

export interface DeployPermissions {
  role: Role;
  canView: boolean;
  canShip: boolean;
  canRollBack: boolean;
}

export interface DeployConsoleState {
  permissions: DeployPermissions;
  // Null until the first read lands, and whenever the caller cannot view.
  status: DeploymentStatus | null;
  loading: boolean;
  // A read failure. Kept separate from `actionError` so a failed rollback does
  // not read as "the status is broken".
  error: string;
  // The outcome of the last action, either way. One field for both because an
  // operator wants the most recent thing that happened, not a log.
  actionMessage: string;
  actionError: string;
  busy: boolean;
  refresh: () => void;
  cut: (bump: SemverBump) => void;
  ship: (deploymentId: string) => void;
  rollBack: (toDeploymentId: string) => void;
}

function describe(err: unknown): string {
  if (err instanceof DeployControlError) {
    // The code is the useful half: "PermissionDenied" tells an operator the
    // request was understood and refused, which is a different problem from a
    // node that does not host the service.
    return `${err.code}: ${err.message}`;
  }
  return err instanceof Error ? err.message : String(err);
}

export function useDeployConsole(env: DeployEnv): DeployConsoleState {
  const { dispatcher } = useCluster();
  const { access } = useMyAccess();
  const role: Role = access?.clusterRole ?? "";

  const permissions: DeployPermissions = {
    role,
    canView: CAN_VIEW.includes(role),
    canShip: CAN_SHIP.includes(role),
    canRollBack: CAN_ROLL_BACK.includes(role),
  };

  const [status, setStatus] = useState<DeploymentStatus | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [actionMessage, setActionMessage] = useState("");
  const [actionError, setActionError] = useState("");
  const [busy, setBusy] = useState(false);
  // Bumped by refresh() and by every completed action -- a deploy that
  // succeeded changes the answer the status read gives.
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (dispatcher === null || !permissions.canView) {
      setStatus(null);
      setLoading(false);
      setError("");
      return;
    }
    const controller = new AbortController();
    let live = true;
    setLoading(true);
    setError("");

    void getDeploymentStatus(dispatcher, env, controller.signal)
      .then((next) => {
        if (live) setStatus(next);
      })
      .catch((err: unknown) => {
        if (live) setError(describe(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
      controller.abort();
    };
    // `permissions` is rebuilt every render, so it is deliberately NOT a
    // dependency -- the boolean inside it is, and a boolean is stable.
    // Depending on the object would re-read the status on every render.
  }, [dispatcher, env, epoch, permissions.canView]);

  // run funnels every action through one place so the busy flag, the message
  // handling and the follow-up refresh cannot be forgotten on the third one.
  const run = useCallback(
    (what: Promise<{ ok: boolean; message: string }> | null) => {
      if (what === null) return;
      setBusy(true);
      setActionMessage("");
      setActionError("");
      void what
        .then((result) => {
          if (result.ok) setActionMessage(result.message || "Done.");
          else setActionError(result.message || "The cluster refused the action.");
        })
        .catch((err: unknown) => setActionError(describe(err)))
        .finally(() => {
          setBusy(false);
          setEpoch((n) => n + 1);
        });
    },
    [],
  );

  const refresh = useCallback(() => setEpoch((n) => n + 1), []);

  const cut = useCallback(
    (bump: SemverBump) => run(dispatcher ? cutVersion(dispatcher, { env, bump }) : null),
    [dispatcher, env, run],
  );

  const ship = useCallback(
    (deploymentId: string) => run(dispatcher ? deployRelease(dispatcher, deploymentId) : null),
    [dispatcher, run],
  );

  const rollBack = useCallback(
    (toDeploymentId: string) =>
      run(dispatcher ? rollbackDeployment(dispatcher, toDeploymentId) : null),
    [dispatcher, run],
  );

  return {
    permissions,
    status,
    loading,
    error,
    actionMessage,
    actionError,
    busy,
    refresh,
    cut,
    ship,
    rollBack,
  };
}
