import { useCallback, useEffect, useState } from "react";
import type { Role } from "@znasllc-io/memql-sdk-core/client";
import {
  DeployControlClient,
  DeployControlError,
  type DeploymentStatus,
} from "@znasllc-io/memql-sdk-core/deploy";

// The two vocabularies this console speaks, declared here because the SDK
// takes them as plain strings. Narrowing them locally keeps a typo out of a
// call the server would reject, without asking the SDK to model an enum the
// wire does not carry.
export type DeployEnv = "staging" | "prod";
export type SemverBump = "major" | "minor" | "patch";

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

/**
 * Renders a failed deploy-console call as the one line an operator reads.
 *
 * Exported for its own test: it is the portal's whole error presentation, and
 * the shape it must NOT produce (the SDK's log-formatted `message`, pasted in
 * after a code the line already carries) is invisible in a rendering test.
 */
export function describeDeployError(err: unknown): string {
  if (err instanceof DeployControlError) {
    // The code is the useful half: "PermissionDenied" tells an operator the
    // request was understood and refused, which is a different problem from a
    // node that does not host the service.
    //
    // engineMessage, not `message`: the latter is shaped for a log line and
    // already contains the code, so pasting it after `${err.code}: ` printed
    // the code twice and the verb once more besides (memql#3339). codeName
    // over the numeric code for the same reason it is the useful half at all
    // -- "PERMISSION_DENIED" says what 7 means.
    return err.engineMessage === "" ? err.codeName : `${err.codeName}: ${err.engineMessage}`;
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

    void new DeployControlClient(dispatcher)
      .getDeploymentStatus(env, { signal: controller.signal })
      .then((next) => {
        if (live) setStatus(next);
      })
      .catch((err: unknown) => {
        if (live) setError(describeDeployError(err));
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
        .catch((err: unknown) => setActionError(describeDeployError(err)))
        .finally(() => {
          setBusy(false);
          setEpoch((n) => n + 1);
        });
    },
    [],
  );

  const refresh = useCallback(() => setEpoch((n) => n + 1), []);

  const cut = useCallback(
    (bump: SemverBump) =>
      run(dispatcher ? new DeployControlClient(dispatcher).cutVersion(env, bump) : null),
    [dispatcher, env, run],
  );

  const ship = useCallback(
    (deploymentId: string) =>
      run(dispatcher ? new DeployControlClient(dispatcher).deploy(deploymentId) : null),
    [dispatcher, run],
  );

  const rollBack = useCallback(
    (toDeploymentId: string) =>
      run(
        dispatcher
          ? new DeployControlClient(dispatcher).rollbackDeployment(toDeploymentId)
          : null,
      ),
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
