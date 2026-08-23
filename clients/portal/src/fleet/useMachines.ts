import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { Event, Role, Row } from "@znasllc-io/memql-sdk-core/client";

import { omitBlank } from "../cluster/args";
import { useCluster } from "../cluster/ClusterProvider";
import { useMyAccess } from "../cluster/useMyAccess";
import * as fleet from "./calls";
import { WORKER_REGISTRATION_CONCEPT_ID } from "./concepts";
import { machineFromRow, invocationFromRow, type Invocation, type Machine } from "./rows";
import type { LabelMap } from "./labels";

// The machines screen's state: the live population, plus the four writes.
//
// ===========================================================================
// THE READ IS A NAMED QUERY AND THE LIVENESS IS A SUBSCRIPTION
// ===========================================================================
// Not useConceptRows (the generic keyset browse most of the portal uses). The
// two reads here are caller-scoped by CONSTRUCTION -- myWorkersWithStatus
// filters `ownerUserId==actor.userId`, allWorkersWithStatus opens with
// `actor.isClusterOwner==true` -- and a generic browse would issue neither, so
// the scope toggle would have nothing to toggle.
//
// The subscription is still the graph's. v1:worker:registration declares
// @rowAuthz(owner="ownerUserId", clusterOwner), which is the OWNED tier: the
// same function that admits a row on a read admits it for a stream
// (memql#4309), so what arrives here is already gated and a full payload
// rather than an id-only notification.
//
// ===========================================================================
// EVENTS ARE FOLDED IN, NOT USED AS A REFETCH TRIGGER
// ===========================================================================
// A heartbeat bumps lastSeenAt every 15 seconds PER MACHINE, and every bump is
// an `updated` event. Re-running the named read on each one turns a ten-machine
// fleet into a read every second and a half, forever, on an idle page. So the
// event payload updates the row in place -- the CDC envelope flattens the
// concept fields alongside the intrinsics, which is exactly the shape
// machineFromRow takes -- and a full re-read is reserved for the two cases a
// fold cannot answer: an id-only notification, and an explicit reload.
//
// ===========================================================================
// SCOPE IS ENFORCED ON THE FOLD TOO
// ===========================================================================
// A cluster owner is admitted to every registration row, so their subscription
// carries events for machines they are not currently looking at. Folding those
// into a "my machines" list would quietly repopulate it with the whole cluster
// -- the same leak the scope toggle exists to make deliberate. Hence the
// ownerUserId check below: the fold applies the same predicate the read did.

export type MachineScope = "mine" | "all";

export interface MachinesState {
  machines: Machine[];
  scope: MachineScope;
  setScope: (scope: MachineScope) => void;
  // Whether the cluster resolved this connection as a cluster owner. Decides
  // what the page OFFERS; the engine decides what it returns.
  isClusterOwner: boolean;
  accessResolved: boolean;
  role: Role;
  userId: string;
  loading: boolean;
  error: string;
  // Set when the CDC subscription could not be opened. Kept separate from
  // `error` for useConceptRows' reason: a successful read must not erase a
  // "live updates are off" notice, or the list looks live moments after going
  // deaf.
  liveDegraded: string;
  reload: () => void;
  // The id of the machine a write is in flight for, or "".
  busyId: string;
  actionError: string;
  // All three resolve TRUE on success and FALSE on refusal.
  //
  // The boolean is not decoration: the label editor renders optimistically
  // (the chip appears before the row comes back) and has to roll that back
  // when the write did not happen. A void-returning write would leave a chip
  // on screen claiming a label the router will never match on.
  rename: (registrationId: string, displayName: string) => Promise<boolean>;
  setOperatorLabels: (registrationId: string, labels: LabelMap) => Promise<boolean>;
  revoke: (registrationId: string, reason: string) => Promise<boolean>;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// eventRow reads a CDC envelope as the row shape machineFromRow takes. Returns
// null for an id-only notification, which the caller answers with a re-read.
function eventRow(event: Event): Row | null {
  if (event.payloadOmitted) return null;
  return event.payload ?? null;
}

export function useMachines(): MachinesState {
  const { query, subscriptions } = useCluster();
  const { access, loading: accessLoading } = useMyAccess();
  const [scope, setScope] = useState<MachineScope>("mine");
  const [machines, setMachines] = useState<Machine[]>([]);
  // Starts true for useArtifacts' reason: a read is effectively in flight from
  // mount, so "loading" is the honest initial state. "Confirmed empty" is what
  // false would claim before any read has been attempted, and this page's empty
  // state reads as "you have no machines", which would be a lie.
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [liveDegraded, setLiveDegraded] = useState("");
  const [epoch, setEpoch] = useState(0);
  const [busyId, setBusyId] = useState("");
  const [actionError, setActionError] = useState("");

  const userId = access?.userId ?? "";
  const role: Role = access?.clusterRole ?? "";
  const isClusterOwner = role === "owner";
  const accessResolved = !accessLoading && access !== null;

  // A non-owner can never be in the "all" scope, even by a stale state update
  // -- the engine would answer empty and the page would say "no machines in
  // this cluster", which is the wrong sentence for "you may not ask".
  const effectiveScope: MachineScope = isClusterOwner ? scope : "mine";

  // The scope the rows currently on screen were read under. See the read
  // effect for what it decides.
  const readScope = useRef<MachineScope>(effectiveScope);

  // ---- the read ---------------------------------------------------------
  useEffect(() => {
    if (query === null) {
      // Not connected yet. NOT a definitive "no machines" answer, so `loading`
      // is left exactly as it is rather than forced false -- see the useState
      // above.
      return;
    }
    let live = true;
    setLoading(true);
    setError("");
    // A SCOPE change clears the list; a reload does not. The rows on screen
    // belong to the scope they were read under, so keeping them across a
    // switch renders another person's machines for a beat under the heading
    // "Your machines" -- which is the one thing this toggle must never do.
    // A reload is the same scope, so holding the rows is the honest render
    // and avoids a flash of empty.
    if (readScope.current !== effectiveScope) {
      readScope.current = effectiveScope;
      setMachines([]);
    }

    const read =
      effectiveScope === "all"
        ? fleet.allWorkersWithStatus(query)
        : fleet.myWorkersWithStatus(query);

    void read
      .then((result) => {
        if (!live) return;
        setMachines(result.rows().map(machineFromRow));
      })
      .catch((err: unknown) => {
        if (live) setError(describe(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, effectiveScope, epoch]);

  // ---- the live fold ----------------------------------------------------
  // Held in refs rather than passed as effect dependencies: the subscription
  // must survive a scope change and a write, and re-subscribing on every one
  // of those is a window during which events are silently missed.
  const scopeRef = useRef(effectiveScope);
  scopeRef.current = effectiveScope;
  const userIdRef = useRef(userId);
  userIdRef.current = userId;

  useEffect(() => {
    if (subscriptions === null) {
      setLiveDegraded("");
      return;
    }
    let live = true;
    let unsubscribe: (() => void) | null = null;

    try {
      unsubscribe = subscriptions.subscribeGraph(
        (event) => {
          if (!live) return;
          const row = eventRow(event);
          if (row === null) {
            // An id-only notification. The row is not in hand, so the only
            // honest answer is the authorized read -- the same one this hook
            // already performs.
            setEpoch((n) => n + 1);
            return;
          }
          const machine = machineFromRow(row);
          if (machine.id === "") return;
          // The same predicate the read applied. See the header.
          if (scopeRef.current === "mine" && machine.ownerUserId !== userIdRef.current) return;

          setMachines((current) => {
            if (event.kind.toLowerCase().includes("deleted")) {
              return current.filter((held) => held.id !== machine.id);
            }
            const at = current.findIndex((held) => held.id === machine.id);
            if (at < 0) return [...current, machine];
            const next = [...current];
            next[at] = machine;
            return next;
          });
        },
        {
          concept: WORKER_REGISTRATION_CONCEPT_ID,
          actions: ["created", "updated", "deleted"],
        },
      );
      setLiveDegraded("");
    } catch (err) {
      // A failed subscribe does NOT break the page -- ordinary reads still
      // work on the same connection. It breaks the PROMISE that the list is
      // live, and that has to be said out loud rather than inferred from a
      // machine that never comes back online on screen.
      setLiveDegraded(describe(err));
    }

    return () => {
      live = false;
      unsubscribe?.();
    };
  }, [subscriptions]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);

  // ---- the writes -------------------------------------------------------
  //
  // None of them refetches on success. The subscription is what carries the
  // new value back, and forcing a read here would paper over that path working
  // rather than exercising it -- the decision useSites.ts documents for
  // createSite.
  const runWrite = useCallback(
    async (registrationId: string, work: () => Promise<unknown>): Promise<boolean> => {
      setBusyId(registrationId);
      setActionError("");
      try {
        await work();
        return true;
      } catch (err: unknown) {
        setActionError(describe(err));
        return false;
      } finally {
        setBusyId("");
      }
    },
    [],
  );

  const rename = useCallback(
    (registrationId: string, displayName: string) => {
      if (query === null) return Promise.resolve(false);
      return runWrite(registrationId, () =>
        fleet.renameWorker(query, registrationId, displayName),
      );
    },
    [query, runWrite],
  );

  const setOperatorLabels = useCallback(
    (registrationId: string, labels: LabelMap) => {
      if (query === null) return Promise.resolve(false);
      return runWrite(registrationId, () =>
        fleet.setWorkerOperatorLabels(query, registrationId, labels),
      );
    },
    [query, runWrite],
  );

  const revoke = useCallback(
    (registrationId: string, reason: string) => {
      if (query === null) return Promise.resolve(false);
      return runWrite(registrationId, () =>
        // revokeWorker predates this epic, so it IS on the generated typed
        // surface and is called the way every other write in the portal is.
        query.revokeWorker({
          registrationId,
          revokedAt: new Date().toISOString(),
          ...(omitBlank(userId) === undefined ? {} : { revokedBy: userId }),
          ...(omitBlank(reason.trim()) === undefined ? {} : { revokeReason: reason.trim() }),
        }),
      );
    },
    [query, runWrite, userId],
  );

  return {
    machines,
    scope: effectiveScope,
    setScope,
    isClusterOwner,
    accessResolved,
    role,
    userId,
    loading,
    error,
    liveDegraded,
    reload,
    busyId,
    actionError,
    rename,
    setOperatorLabels,
    revoke,
  };
}

// ---------------------------------------------------------------------------
// One machine's recent calls
// ---------------------------------------------------------------------------

export interface MachineActivityState {
  invocations: Invocation[];
  loading: boolean;
  error: string;
}

// Read on demand -- the activity list opens per machine, and reading every
// machine's history to render a collapsed row would be twenty-five rows of
// telemetry per machine nobody has expanded.
//
// `enabled` rather than a conditional call: hook order cannot vary between
// renders (the parameter useMyAccess takes, for the same reason).
export function useMachineActivity(
  workerId: string,
  enabled: boolean,
  asOperator: boolean,
): MachineActivityState {
  const { query } = useCluster();
  const [invocations, setInvocations] = useState<Invocation[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    if (query === null || workerId === "" || !enabled) {
      setInvocations([]);
      setLoading(false);
      setError("");
      return;
    }
    let live = true;
    setLoading(true);
    setError("");

    void fleet
      .invocationsForWorker(query, workerId, asOperator)
      .then((result) => {
        if (live) setInvocations(result.rows().map(invocationFromRow));
      })
      .catch((err: unknown) => {
        if (live) setError(describe(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, workerId, enabled, asOperator]);

  return useMemo(
    () => ({ invocations, loading, error }),
    [invocations, loading, error],
  );
}
