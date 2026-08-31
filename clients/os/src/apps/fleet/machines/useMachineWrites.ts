import { useCallback, useState } from "react";

import { useSession } from "../../../chrome/access";
import { useOsConnection } from "../../../live/connection";
import type { LabelMap } from "../labels";

// The three writes the Machines directory makes, and the one busy/error pair
// they share.
//
// ===========================================================================
// NONE OF THEM REFETCHES
// ===========================================================================
// The subscription carries the new value back: v1:worker:registration
// declares @rowAuthz(owner="ownerUserId", clusterOwner), and row admission
// gates a stream exactly as it gates a read (memql#4309), so an accepted
// write arrives as a full-payload `updated` event on the same feed the list
// renders. Re-reading after each write would double every call and still
// race the event.
//
// The old hole in that model -- a dropped event making an operator's own
// write look permanently ignored, with the list still rendering as live --
// is closed by the collection, which re-seeds on any gap or reconnect. A lost
// echo costs a re-read rather than a wrong screen.
//
// ===========================================================================
// EVERY WRITE RESOLVES TO A BOOLEAN
// ===========================================================================
// Not void. The label editor renders optimistically -- a chip appears before
// the row comes back -- and has to roll that back when the write did not
// happen. A void-returning write would leave a chip on screen claiming a
// label the router will never match on, which is the exact class of silent
// disagreement the two-label-map split exists to prevent.

export interface MachineWrites {
  /** The id of the machine a write is in flight for, or "". */
  busyId: string;
  /** The last refusal, in the server's own words. Cleared when a write
   *  starts, so it is never read as belonging to the current attempt. */
  actionError: string;
  rename: (registrationId: string, displayName: string) => Promise<boolean>;
  setOperatorLabels: (registrationId: string, labels: LabelMap) => Promise<boolean>;
  revoke: (registrationId: string, reason: string) => Promise<boolean>;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export function useMachineWrites(): MachineWrites {
  const connection = useOsConnection();
  const { access } = useSession();
  const [busyId, setBusyId] = useState("");
  const [actionError, setActionError] = useState("");

  const run = useCallback(
    async (registrationId: string, write: () => Promise<unknown>): Promise<boolean> => {
      if (connection === null) {
        setActionError("Not connected to the cluster, so nothing was written.");
        return false;
      }
      setBusyId(registrationId);
      setActionError("");
      try {
        await write();
        return true;
      } catch (err: unknown) {
        setActionError(describe(err));
        return false;
      } finally {
        setBusyId("");
      }
    },
    [connection],
  );

  // Writes `displayName`. `name` stays the cockpit's hostname, which the next
  // reconnect re-stamps -- which is the whole reason a rename needs its own
  // field rather than overwriting the reported one.
  const rename = useCallback(
    (registrationId: string, displayName: string) =>
      run(registrationId, () =>
        connection!.query.renameWorker({ registrationId, displayName }),
      ),
    [connection, run],
  );

  // The whole map is REPLACED, not merged: the editor edits the set as a set,
  // and a merge would make removing a label impossible through this surface.
  const setOperatorLabels = useCallback(
    (registrationId: string, labels: LabelMap) =>
      run(registrationId, () =>
        connection!.query.setWorkerOperatorLabels({ registrationId, operatorLabels: labels }),
      ),
    [connection, run],
  );

  // Revocation is an UPDATE, not a delete: the row is audit history, and its
  // credential's hash must stay taken. `revokedBy` is the caller when we know
  // who that is -- a self-revocation from an unresolved session omits it
  // rather than guessing, because a wrong id in an audit field is worse than
  // an empty one.
  const revoke = useCallback(
    (registrationId: string, reason: string) => {
      const trimmedReason = reason.trim();
      const revokedBy = access?.userId ?? "";
      return run(registrationId, () =>
        connection!.query.revokeWorker({
          registrationId,
          revokedAt: new Date().toISOString(),
          ...(revokedBy === "" ? {} : { revokedBy }),
          ...(trimmedReason === "" ? {} : { revokeReason: trimmedReason }),
        }),
      );
    },
    [access, connection, run],
  );

  return { busyId, actionError, rename, setOperatorLabels, revoke };
}
