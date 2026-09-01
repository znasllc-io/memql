import { useCallback, useState } from "react";

import { useOsConnection } from "../../live/connection";
import type { ChunkDecision } from "./concepts";

// The one write this app makes.
//
// ===========================================================================
// NOTHING HERE CHECKS A ROLE, AND NOTHING HERE IS THE AUTHORIZATION
// ===========================================================================
// The manifest's `writer` gate hides this app from a reader because showing a
// review queue nobody can decide teaches nothing; that is presentation (spec
// section E), and editing a boolean in a browser changes none of it.
//
// What the ENGINE does is narrower to say honestly:
// `v1:knowledge:documentChunk` declares no `@rowAuthz` tier and carries no
// owner field to declare one against, so `setChunkValidationStatus` is
// admitted for any authenticated caller -- the standing residual its three
// sibling mutations already sit on, recorded in the per-row-authz audit. This
// app inherits that; it does not widen it, and it does not pretend the role
// gate is the boundary.
//
// ===========================================================================
// A REFUSAL RENDERS ON THE CARD THAT PRODUCED IT, NEVER AS A TOAST
// ===========================================================================
// Which is why the busy key and the refusal are keyed BY CHUNK. A shared pair
// would put one card's refusal under another card somebody was looking at,
// naming a failure they did not cause -- and in a list of near-identical cards
// that is not a cosmetic problem, it is a wrong answer.

export interface ChunkDecisions {
  /** The chunk id a write is in flight for, or "". */
  busyChunkId: string;
  /** The last refusal, as (chunkId -> the server's own sentence). Cleared for
   *  a chunk when a fresh attempt on it starts. */
  refusals: Readonly<Record<string, string>>;
  /**
   * Decide one chunk. Resolves true when the engine accepted it.
   *
   * The caller updates the card FROM THIS RESULT rather than optimistically:
   * `v1:knowledge:*` carries no broadcast routing, so nothing would correct an
   * optimistic flip that the engine had in fact refused -- the card would read
   * as approved forever, and the chunk would stay out of retrieval.
   */
  decide: (chunkId: string, status: ChunkDecision) => Promise<boolean>;
}

export function useChunkDecisions(): ChunkDecisions {
  const connection = useOsConnection();
  const [busyChunkId, setBusyChunkId] = useState("");
  const [refusals, setRefusals] = useState<Record<string, string>>({});

  const decide = useCallback(
    async (chunkId: string, status: ChunkDecision): Promise<boolean> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setRefusals((held) => ({
          ...held,
          [chunkId]: "Not connected to the cluster, so nothing was written.",
        }));
        return false;
      }
      setBusyChunkId(chunkId);
      setRefusals((held) => {
        const next = { ...held };
        delete next[chunkId];
        return next;
      });
      try {
        await query.setChunkValidationStatus({ chunkId, status });
        return true;
      } catch (err: unknown) {
        setRefusals((held) => ({
          ...held,
          [chunkId]: err instanceof Error ? err.message : String(err),
        }));
        return false;
      } finally {
        setBusyChunkId("");
      }
    },
    [connection],
  );

  return { busyChunkId, refusals, decide };
}
