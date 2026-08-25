import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { getRowByConceptAndId, type Role, type Row } from "@znasllc-io/memql-sdk-core/client";

import { omitBlank } from "../cluster/args";
import { useCluster } from "../cluster/ClusterProvider";
import { useLive } from "../cluster/useLive";
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
// THE FOLD IS THE SDK'S NOW (memql#4539). This file used to carry its own --
// a splice over an array, with the delete branch decided by string-sniffing
// `event.kind.toLowerCase().includes("deleted")`, one renamed topic away from
// folding deletes as updates. It is `LiveCollection` underneath: same
// behaviour, matched on the decoded enum, and shared with every other live
// surface so the next fix lands once.
//
// ===========================================================================
// SCOPE IS ENFORCED ON THE FOLD TOO
// ===========================================================================
// A cluster owner is admitted to every registration row, so their subscription
// carries events for machines they are not currently looking at. Folding those
// into a "my machines" list would quietly repopulate it with the whole cluster
// -- the same leak the scope toggle exists to make deliberate. That predicate
// is the collection's `inScope` hook, applied to every folded row -- including
// an UPDATE that moves a row out of scope, which the old splice ignored.
//
// ===========================================================================
// THE WRITE'S ECHO IS NOW GAP-SAFE
// ===========================================================================
// None of the writes below refetches: the subscription carries the new value
// back. That model had one hole -- a dropped event made the operator's own
// write look ignored, permanently, with the page still rendering as live. The
// collection re-seeds on any gap or reconnect, so a lost echo costs a re-read
// rather than a wrong screen.

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

export function useMachines(): MachinesState {
  const { query } = useCluster();
  const { access, loading: accessLoading } = useMyAccess();
  const [scope, setScope] = useState<MachineScope>("mine");
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

  // The scope is part of the collection KEY, which is what makes a scope
  // change a different collection rather than a re-read of this one. Rows on
  // screen belong to the scope they were read under, so sharing one
  // collection across the toggle would render another person's machines for a
  // beat under the heading "Your machines" -- the one thing this toggle must
  // never do. Two collections, and switching back reuses the first.
  //
  // THE CALLER'S OWN ID IS DELIBERATELY *NOT* IN THE KEY. It arrives on its
  // own round trip (useMyAccess), so putting it there makes the key CHANGE
  // when that lands -- a second collection, seeded from empty, which the
  // page's `loading && machines.length === 0` gate renders as a skeleton. The
  // list is replaced, every card unmounts, and an operator part-way through
  // renaming a machine loses the editor and what they typed. Whether it
  // happened at all came down to which read settled first, so it passed
  // locally and failed on CI.
  //
  // The id is only needed by the FOLD, and the fold reads it through the ref
  // below -- the latest committed value at event time, which is the same
  // reasoning useConceptRows states for its paged-id set.
  const key = query === null ? null : `fleet:machines:${effectiveScope}`;
  const userIdRef = useRef(userId);
  userIdRef.current = userId;

  const live = useLive<Row>(key, () => ({
    concept: WORKER_REGISTRATION_CONCEPT_ID,
    actions: ["created", "updated", "deleted"],
    // One page: both reads are caller-scoped named queries, not a keyset walk.
    paged: false,
    seed: async (_cursor, signal) => {
      if (query === null) return { rows: [], nextCursor: "" };
      const result =
        effectiveScope === "all"
          ? await fleet.allWorkersWithStatus(query, { signal })
          : await fleet.myWorkersWithStatus(query, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    // v1:worker:registration is the OWNED tier, so events arrive with a full
    // payload and this is unused in practice -- present because the tier is a
    // declaration that can change, and a collection that cannot answer an
    // id-only notification loses those rows silently when it does.
    reread: async (rowId, signal) => {
      if (query === null) return null;
      return getRowByConceptAndId(query, WORKER_REGISTRATION_CONCEPT_ID, rowId, { signal });
    },
    // The same predicate the read applied. See the header.
    //
    // Through the REF, so a fold that arrives before access resolves is
    // refused (the id is still "") rather than admitted, and one that arrives
    // after is decided correctly with no re-seed. The seed itself never needed
    // the id: myWorkersWithStatus is scoped server-side on actor.userId.
    inScope: (row) =>
      effectiveScope !== "mine" ||
      (userIdRef.current !== "" && machineFromRow(row).ownerUserId === userIdRef.current),
  }));

  const machines = useMemo(
    () => live.rows.map(machineFromRow).filter((machine) => machine.id !== ""),
    [live.rows],
  );

  // `loading` starts true for useArtifacts' reason: a read is effectively in
  // flight from mount, so it is the honest initial state -- "confirmed empty"
  // is what false would claim before anything was asked, and this page's empty
  // state reads as "you have no machines".
  const loading = query === null || live.state === "seeding";
  // Kept separate from `error` for useConceptRows' reason: a successful read
  // must not erase a "live updates are off" notice, or the list looks live
  // moments after going deaf. `degraded` is a gap being re-seeded;
  // `disconnected` is the stream gone.
  const liveDegraded =
    live.state === "disconnected"
      ? "the connection to the cluster dropped -- these rows are as of the last update"
      : live.state === "degraded"
        ? "live updates were interrupted -- refreshing"
        : "";
  const { error, reload } = live;

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
