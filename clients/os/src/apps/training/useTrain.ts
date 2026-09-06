import { useCallback, useState } from "react";

import { useOsConnection } from "../../live/connection";

// Teaching a domain from a file: the app's second write, and the act the
// surface exists for.
//
// ===========================================================================
// UPLOAD AND TEACH ARE TWO ACTS, AND THAT IS THE DESIGN
// ===========================================================================
// Uploading a file makes it readable and searchable in the Library. It does
// NOT put its contents into a knowledge domain -- nothing an agent answers
// with changes until somebody says which domain this file should teach.
//
// The old surface hid that distinction and got it wrong in the process: it
// uploaded into a chat space, showed a "plan" finishing, and produced no
// knowledge chunks at all. Making the second act explicit is what makes the
// review queue on the next section have anything in it.
//
// `libraryTrainFile(fileId, domainId)` is the engine's own gate: it re-reads
// the file under the CALLER's actor (somebody else's file is simply not
// found), checks the domain write authorizer, joins the file's chunks and
// ingests them as `v1:knowledge:documentChunk` rows with
// `source: "fileUpload"`. Adding a domain twice writes nothing new.
//
// ===========================================================================
// A REFUSAL RENDERS ON THE ROW THAT PRODUCED IT, NEVER AS A TOAST
// ===========================================================================
// Which is why the busy key and the refusal are keyed BY FILE. A shared pair
// would put one row's refusal under another row somebody was looking at,
// naming a failure they did not cause.

export interface TrainAct {
  /** The file id a write is in flight for, or "". */
  busyFileId: string;
  /** The last refusal, as (fileId -> the server's own sentence). Cleared for
   *  a file when a fresh attempt on it starts. */
  refusals: Readonly<Record<string, string>>;
  /**
   * Teach one domain from one file. Resolves true when the engine accepted it.
   *
   * The caller does NOT update the row from this result. `v1:library:file`
   * carries broadcast routing, and `libraryTrainFile` writes the merged
   * `trainedIntoDomainIds` back through `appendLibraryFileTrainedDomain` --
   * so the row arrives on the feed with the domain on it, from the cluster,
   * which is the same path a teach from somebody's phone would take. An
   * optimistic flip here would be a second source for one fact.
   */
  train: (fileId: string, domainId: string) => Promise<boolean>;
}

export function useTrain(): TrainAct {
  const connection = useOsConnection();
  const [busyFileId, setBusyFileId] = useState("");
  const [refusals, setRefusals] = useState<Record<string, string>>({});

  const train = useCallback(
    async (fileId: string, domainId: string): Promise<boolean> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setRefusals((held) => ({
          ...held,
          [fileId]: "Not connected to the cluster, so nothing was taught.",
        }));
        return false;
      }
      if (domainId.trim() === "") {
        setRefusals((held) => ({ ...held, [fileId]: "Choose a domain first." }));
        return false;
      }
      setBusyFileId(fileId);
      setRefusals((held) => {
        const next = { ...held };
        delete next[fileId];
        return next;
      });
      try {
        await query.libraryTrainFile({ fileId, domainId });
        return true;
      } catch (err: unknown) {
        setRefusals((held) => ({
          ...held,
          [fileId]: err instanceof Error ? err.message : String(err),
        }));
        return false;
      } finally {
        setBusyFileId("");
      }
    },
    [connection],
  );

  return { busyFileId, refusals, train };
}
