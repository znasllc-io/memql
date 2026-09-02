import { useCallback, useState } from "react";
import { getRowByConceptAndId, newShortId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../../live/connection";
import { useLiveCollection, type LiveCollectionHandle } from "../../../live/useLiveCollection";
import { WATCHED_FOLDER_CONCEPT, type NewBackup } from "./rows";

// The backups feed, and the writes over it.
//
// A FOURTH CONCEPT in this app, and not a violation of the one-feed rule: that
// rule is per CONCEPT -- two subscriptions over one concept can disagree about
// what the cluster holds, two concepts cannot. The section reads this feed for
// the arrangement, the app's existing `files` feed for the per-file link
// states, its `folders` feed for the destination names, and the shell-wide
// machines provider for the machine names. Four populations, four owners, no
// duplicates.
//
// `v1:library:watchedFolder` broadcasts created AND updated
// (component/node/routing.go), which the rules had to add: these rows have two
// writers on different nodes -- a browser create lands on a bff, a cockpit's
// sweep report lands wherever its own request dialed -- and without the rules
// this list is correct on load and frozen after. A backup that has stopped
// reporting would then look exactly like one that is fine.

export function useBackupsFeed(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("files:watchedFolders", (connection) => ({
    concept: WATCHED_FOLDER_CONCEPT,
    seed: async (cursor, signal) => {
      // No `workerId` argument: the app wants every backup the person has.
      // The argument exists for the cockpit, which asks about one machine.
      const result = await connection.query.libraryWatchedFolders(
        {},
        { signal, ...(cursor !== "" ? { cursor } : {}) },
      );
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, WATCHED_FOLDER_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
  }));
}

/** The busy key a CREATE reports under.
 *
 * A create has no row id yet -- the row does not exist -- and "" is already
 * the value busyId carries when nothing is in flight at all. Colliding those
 * two would leave the create button unable to say it is working. The colon
 * makes it impossible for a minted short id to collide with it.
 */
export const CREATE_BUSY_KEY = "backup:new";

export interface BackupWrites {
  /** The id of the row a write is in flight for, or "" -- so one row's
   *  spinner never disables another's controls. */
  busyId: string;
  /** The server's own sentence when a write was refused, or "". */
  actionError: string;
  /**
   * WHICH write the standing error belongs to -- a row id, or CREATE_BUSY_KEY.
   *
   * The error and the id travel together because one shared error string over
   * a list of rows has no home: rendered on every row it is four copies of one
   * refusal, and rendered on none it is a write that failed silently. The
   * first version of this surface gated the notice on the stop-confirm being
   * open, which meant a refused Pause said nothing at all.
   */
  errorId: string;
  clearError: () => void;
  create: (spec: NewBackup) => Promise<boolean>;
  setStatus: (id: string, status: "active" | "paused") => Promise<boolean>;
  update: (id: string, patch: Omit<NewBackup, "workerId" | "localPath">) => Promise<boolean>;
  stop: (id: string) => Promise<boolean>;
}

/**
 * The writes, in the shape apps/fleet/machines/useMachineWrites.ts established.
 *
 * NO WRITE REFETCHES. Row authz gates the subscription exactly as it gates a
 * read, so an accepted write arrives back as an `updated` event on the feed
 * this app already holds -- a re-read after every write would double the
 * traffic and still be behind the event.
 *
 * EVERY WRITE RESOLVES TO A BOOLEAN rather than void, so a caller can tell a
 * refusal from a success without reading the error slot, and so a form knows
 * whether to close itself.
 *
 * THE REFUSAL IS THE SERVER'S OWN SENTENCE, verbatim. This shell has no toasts
 * deliberately; a refusal renders beside the control that produced it, and
 * re-wording it would lose the part that says which path or which machine.
 */
export function useBackupWrites(): BackupWrites {
  const connection = useOsConnection();
  const [busyId, setBusyId] = useState("");
  const [actionError, setActionError] = useState("");
  const [errorId, setErrorId] = useState("");

  const run = useCallback(
    async (id: string, work: (query: NonNullable<typeof connection>["query"]) => Promise<unknown>) => {
      const query = connection?.query ?? null;
      if (query === null) {
        setActionError("Not connected to the cluster, so nothing was changed.");
        setErrorId(id);
        return false;
      }
      setBusyId(id);
      setActionError("");
      setErrorId("");
      try {
        await work(query);
        return true;
      } catch (err) {
        setActionError(err instanceof Error ? err.message : String(err));
        setErrorId(id);
        return false;
      } finally {
        setBusyId("");
      }
    },
    [connection],
  );

  const create = useCallback(
    async (spec: NewBackup) => {
      // The id is minted HERE, client-side (the mutationCreateSpace pattern):
      // the live feed delivers the row, so the create does not need to hand
      // one back and the form does not need a second read to find it.
      const watchId = newShortId();
      return run(CREATE_BUSY_KEY, (query) =>
        query.createLibraryWatchedFolder({
          watchId,
          workerId: spec.workerId,
          localPath: spec.localPath,
          ...(spec.folderId !== "" ? { folderId: spec.folderId } : {}),
          ...(spec.excludeGlobs.length > 0 ? { excludeGlobs: spec.excludeGlobs } : {}),
          includeHidden: spec.includeHidden,
        }),
      );
    },
    [run],
  );

  const setStatus = useCallback(
    async (id: string, status: "active" | "paused") =>
      run(id, (query) => query.setLibraryWatchedFolderStatus({ watchId: id, status })),
    [run],
  );

  const update = useCallback(
    async (id: string, patch: Omit<NewBackup, "workerId" | "localPath">) =>
      run(id, (query) =>
        // folderId and excludeGlobs are sent even when empty: this mutation is
        // a read-merge, and "" is how a person says "back to the Library root"
        // or "stop skipping anything". Omitting them would make clearing a
        // value impossible, which is the trap omitBlank exists to avoid on
        // forms where the opposite is true.
        query.updateLibraryWatchedFolder({
          watchId: id,
          folderId: patch.folderId,
          excludeGlobs: patch.excludeGlobs,
          includeHidden: patch.includeHidden,
        }),
      ),
    [run],
  );

  const stop = useCallback(
    async (id: string) => run(id, (query) => query.archiveLibraryWatchedFolder({ watchId: id })),
    [run],
  );

  return {
    busyId,
    actionError,
    errorId,
    clearError: () => {
      setActionError("");
      setErrorId("");
    },
    create,
    setStatus,
    update,
    stop,
  };
}
