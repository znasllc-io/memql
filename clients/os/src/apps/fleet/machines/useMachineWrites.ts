import { useCallback, useState } from "react";

import { useOsConnection } from "../../../live/connection";
import type { LabelMap } from "../labels";
import type { SharingMode } from "../rows";

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

/**
 * What a removal actually did.
 *
 * REMOVAL IS TWO WRITES AND EITHER CAN LAND ALONE, so the receipt names both
 * rather than collapsing to a boolean (epic memql#5327, design D2). Before
 * this the shell made one call: it revoked the registration row and left the
 * credential active, so a machine that had been "removed" kept its stream and
 * could come back. The engine does both in one act now, and the honest report
 * of a partial result belongs on screen rather than in a log -- somebody
 * removing a stolen laptop needs to know whether its token is still live.
 */
export interface RemovalReceipt {
  /** What happened to the credential. `revoked` is the whole job done;
   *  `revoke_failed` means the machine is out of the fleet but its token still
   *  exists; `not_found` means none was bound to it; `unknown` means the
   *  receipt did not say. */
  credentialState: "revoked" | "revoke_failed" | "not_found" | "unknown";
  /** True when the machine had already been removed before this act. */
  alreadyRevoked: boolean;
  /** The engine's own sentence, shown verbatim. */
  sentence: string;
}

/**
 * What the owner decides about who may use their machine (epic memql#5344).
 * Under `owner` and `cluster` the lists are sent EMPTY whatever the caller
 * put in them: a list saved under another mode is residue a later reader
 * could honour by mistake (design G10).
 */
export interface ShareChoice {
  mode: "owner" | "people" | "cluster";
  userIds: string[];
  groupIds: string[];
}

/**
 * What a sharing write did, in the engine's words.
 *
 * THE SENTENCE IS THE ENGINE'S, and it is the only success this surface
 * shows: it names the cockpit's half when that is still missing, which is the
 * difference between "lent" and "lent, and serving".
 */
export interface SharingReceipt {
  mode: SharingMode;
  /** How many people and groups the stored list now names. */
  people: number;
  groups: number;
  sentence: string;
}

function countFrom(v: unknown): number {
  return typeof v === "number" && Number.isFinite(v) && v > 0 ? Math.floor(v) : 0;
}

/**
 * Read the engine's sharing receipt off the answer row.
 *
 * A row that does not parse still reports what WAS done -- we are past the
 * throw, so the write landed -- but claims nothing it cannot read: the mode is
 * the one sent, and the sentence says the cluster did not describe the result.
 */
function sharingReceiptFrom(row: unknown, sent: ShareChoice): SharingReceipt {
  const fields = row !== null && typeof row === "object" ? (row as Record<string, unknown>) : {};
  const mode = fields["mode"];
  const sentence = fields["sentence"];
  return {
    mode: mode === "owner" || mode === "people" || mode === "cluster" ? mode : sent.mode,
    people: countFrom(fields["people"]),
    groups: countFrom(fields["groups"]),
    sentence:
      typeof sentence === "string" && sentence.trim() !== ""
        ? sentence
        : "Saved. The cluster did not say what changed.",
  };
}

export interface MachineWrites {
  /** The id of the machine a write is in flight for, or "". */
  busyId: string;
  /** The last refusal, in the server's own words. Cleared when a write
   *  starts, so it is never read as belonging to the current attempt. */
  actionError: string;
  rename: (registrationId: string, displayName: string) => Promise<boolean>;
  setOperatorLabels: (registrationId: string, labels: LabelMap) => Promise<boolean>;
  /** Remove a machine: its registration AND its credential, as one act.
   *  Resolves to the receipt on success, or null on a refusal with
   *  `actionError` carrying the reason. */
  revoke: (registrationId: string, reason: string) => Promise<RemovalReceipt | null>;
  /** The OWNER's half of the sharing consent (epic memql#5146, D6; three
   *  modes since epic memql#5344). The cockpit's half comes from that
   *  machine's own policy.yaml and is not writable from here -- deliberately:
   *  it is a decision about where the machine is, and only the machine can
   *  make it. Resolves to the engine's receipt, or null on a refusal with
   *  `actionError` carrying the engine's words. */
  setSharing: (registrationId: string, choice: ShareChoice) => Promise<SharingReceipt | null>;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/**
 * Read the engine's removal receipt off the answer row.
 *
 * A row that does not parse reports `unknown` rather than success. The act
 * itself worked -- we are past the throw -- but what cannot be read must not
 * be claimed, and "removed, and the cluster did not say what happened to its
 * credential" is the honest sentence for a receipt this page did not
 * understand.
 */
function receiptFrom(row: unknown): RemovalReceipt {
  const fallback: RemovalReceipt = {
    credentialState: "unknown",
    alreadyRevoked: false,
    sentence: "Removed from the fleet. The cluster did not say what happened to its credential.",
  };
  if (row === null || typeof row !== "object") return fallback;
  const fields = row as Record<string, unknown>;
  const state = fields["credentialState"];
  const sentence = fields["sentence"];
  return {
    credentialState:
      state === "revoked" || state === "revoke_failed" || state === "not_found" ? state : "unknown",
    alreadyRevoked: fields["alreadyRevoked"] === true,
    sentence: typeof sentence === "string" && sentence !== "" ? sentence : fallback.sentence,
  };
}

export function useMachineWrites(): MachineWrites {
  const connection = useOsConnection();
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

  // ONE CALL, TWO WRITES (epic memql#5327, design D2).
  //
  // This rendered `revokeWorker` and nothing else, so the registration was
  // excluded from routing while the TOKEN stayed active: the machine kept its
  // stream, kept heartbeating onto a revoked row, and could re-register the
  // moment a new one appeared. An operator told "removed" had removed half of
  // it, and the half left behind is the half that matters.
  //
  // The shell could have made a second call, and then it would own the window
  // between them -- a reload mid-flight would leave half a removal standing.
  // fleetRevokeMachine does both server-side and returns a receipt naming
  // which halves landed.
  //
  // Revocation is still an UPDATE, not a delete: the row is audit history and
  // its credential's hash must stay taken.
  //
  // `revokedBy` is no longer sent. The engine stamps it from the actor, which
  // is the rule sharing already follows and for its reason: the record of WHO
  // removed a machine must not be writable by whoever is holding the keyboard.
  const revoke = useCallback(
    async (registrationId: string, reason: string): Promise<RemovalReceipt | null> => {
      if (connection === null) {
        setActionError("Not connected to the cluster, so nothing was written.");
        return null;
      }
      setBusyId(registrationId);
      setActionError("");
      try {
        const result = await connection.query.fleetRevokeMachine({
          registrationId,
          ...(reason.trim() === "" ? {} : { reason: reason.trim() }),
        });
        return receiptFrom(result.rows()[0]);
      } catch (err: unknown) {
        setActionError(describe(err));
        return null;
      } finally {
        setBusyId("");
      }
    },
    [connection],
  );

  // `sharedAt` and `sharedBy` are stamped by the mutation from the clock and
  // the actor, never sent from here: the record of WHO shared a machine must
  // not be writable by whoever is holding the keyboard.
  // Through the BUILTIN, not the mutation (epic memql#5327, finding M-2).
  // setWorkerSharing is @serverOnly now: v1:worker:registration declares the
  // composite owner/clusterOwner tier, and the write guard grants the
  // cluster-owner escape on it -- so an operator could lend hardware they do
  // not own, and un-lend hardware somebody else had lent. fleetSetSharing
  // resolves the machine through the caller's OWN machines first.
  //
  // A RECEIPT, NOT A BOOLEAN (epic memql#5344). The panel shows the engine's
  // own sentence as the only success it claims, and that sentence names the
  // cockpit's half when it is still missing -- so the answer row is read
  // rather than discarded. BOTH LISTS ARE ALWAYS SENT, empty outside
  // `people`: the engine ignores them there, and a write that left them out
  // would ask the reader to know that.
  const setSharing = useCallback(
    async (registrationId: string, choice: ShareChoice): Promise<SharingReceipt | null> => {
      if (connection === null) {
        setActionError("Not connected to the cluster, so nothing was written.");
        return null;
      }
      const sent: ShareChoice =
        choice.mode === "people"
          ? { mode: "people", userIds: [...choice.userIds], groupIds: [...choice.groupIds] }
          : { mode: choice.mode, userIds: [], groupIds: [] };
      setBusyId(registrationId);
      setActionError("");
      try {
        const result = await connection.query.fleetSetSharing({ registrationId, ...sent });
        return sharingReceiptFrom(result.rows()[0] ?? null, sent);
      } catch (err: unknown) {
        setActionError(describe(err));
        return null;
      } finally {
        setBusyId("");
      }
    },
    [connection],
  );

  return { busyId, actionError, rename, setOperatorLabels, revoke, setSharing };
}
