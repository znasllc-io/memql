import { useCallback, useEffect, useRef, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { chunkAwaitsReview, chunkFromRow, type Chunk } from "./rows";

// The review queue: what the pipeline learned, held for a person before it
// enters retrieval.
//
// ===========================================================================
// WHY THIS WALKS DOMAINS RATHER THAN READING A QUEUE
// ===========================================================================
// There is no "unvalidated chunks" query, and adding one is not free: a filter
// on `validationStatus` needs an index behind it or it is a full scan per
// page, and that is a database decision rather than an app one. What the tree
// HAS is `documentChunksForDomain` -- keyset-paginated at 50, newest first --
// and `allDocumentChunkDomains`, which now projects `validationStatus`
// (memql#4740).
//
// So the walk is: ask the rollup which domains actually hold unvalidated
// chunks, and page only those. That second half is the whole payoff of the
// shape change -- without it this hook would have to page EVERY domain to find
// out whether any of them had work in it.
//
// ===========================================================================
// A PAGE OF FIFTY MAY CONTAIN NO WORK, AND THAT MUST NOT LOOK LIKE A BUG
// ===========================================================================
// The read is not filtered on validation status -- it returns a domain's
// newest fifty chunks, and in a domain that was seeded once and reviewed
// months ago the first page is fifty validated rows. A "load more" that
// fetched one page and added nothing would read as a dead button.
//
// So one step keeps pulling until it has something to show, bounded by
// PAGES_PER_STEP. The bound is what stops a single click walking a
// hundred-thousand-row corpus; the caption says how many pages were read, and
// never claims a total it has not seen.

/** Pages one `loadMore` will walk before returning empty-handed. */
export const PAGES_PER_STEP = 8;

export interface ReviewQueue {
  /** Every chunk this queue has loaded, in read order. A decided chunk STAYS
   *  -- the card collapses in place rather than vanishing under the cursor. */
  chunks: Chunk[];
  state: "idle" | "loading" | "ready" | "error";
  /** The server's own sentence when a read failed. "" otherwise. */
  error: string;
  /** Pages actually read. The caption's number; never a total. */
  pagesRead: number;
  /** True when every page of every domain with work has been walked. */
  exhausted: boolean;
  loadMore: () => void;
  reload: () => void;
  /** Record a decision locally, from the reply. */
  applyDecision: (chunkId: string, status: string) => void;
}

interface Walk {
  domainIds: string[];
  index: number;
  cursor: string;
}

/**
 * `domainIds` is the ORDERED list of domains to walk.
 *
 * It is passed in rather than read here, because the Domains feed already
 * holds that answer: two reads of `allDocumentChunkDomains` would be free to
 * disagree about which domains exist, and this app would then have a queue
 * that walked domains its own Domains list did not show.
 *
 * A CHANGE TO ITS CONTENT RESTARTS THE WALK. The list is joined into a string
 * and that string is the dependency, rather than the array: the array is
 * derived on every render of the parent, so its identity says nothing about
 * whether the set changed, while its content is exactly what decides what
 * gets read.
 */
export function useReviewQueue(domainIds: readonly string[]): ReviewQueue {
  const connection = useOsConnection();
  const [chunks, setChunks] = useState<Chunk[]>([]);
  const [state, setState] = useState<ReviewQueue["state"]>("idle");
  const [error, setError] = useState("");
  const [pagesRead, setPagesRead] = useState(0);
  const [exhausted, setExhausted] = useState(false);
  const [attempt, setAttempt] = useState(0);

  const walk = useRef<Walk>({ domainIds: [], index: 0, cursor: "" });
  // Guards a second step while one is in flight. A ref rather than the `state`
  // value, because two clicks in one frame both read the same stale state.
  const busy = useRef(false);
  const generation = useRef(0);

  const domainKey = domainIds.join(" ");

  const step = useCallback(async () => {
    if (connection === null || busy.current) return;
    const mine = generation.current;
    busy.current = true;
    setState("loading");
    try {
      const found: Chunk[] = [];
      let pages = 0;
      while (pages < PAGES_PER_STEP) {
        const held = walk.current;
        const domainId = held.domainIds[held.index];
        if (domainId === undefined) break;
        const result = await connection.query.documentChunksForDomain(
          { domainId },
          held.cursor === "" ? {} : { cursor: held.cursor },
        );
        if (mine !== generation.current) return;
        pages += 1;
        for (const row of result.rows() as Row[]) {
          const chunk = chunkFromRow(row);
          if (chunkAwaitsReview(chunk)) found.push(chunk);
        }
        // An EMPTY cursor means the set is exhausted: it is minted only on a
        // full page, so this is the engine saying "that was the last of them"
        // rather than a value that failed to arrive.
        const next = result.meta()?.cursor ?? "";
        walk.current =
          next === ""
            ? { domainIds: held.domainIds, index: held.index + 1, cursor: "" }
            : { domainIds: held.domainIds, index: held.index, cursor: next };
        if (found.length > 0) break;
      }
      if (mine !== generation.current) return;
      // EXHAUSTION IS DERIVED FROM WHERE THE WALK ENDED, not from having
      // looped once more and found nothing. Reading it off the top of the loop
      // meant the last page always left "Load more" on screen for one more
      // click that could only return empty -- a control offering a read the
      // hook already knows the answer to.
      setExhausted(walk.current.index >= walk.current.domainIds.length);
      setPagesRead((n) => n + pages);
      if (found.length > 0) {
        // De-duplicated against what is already held: a reload and a step can
        // both reach the same row, and a queue that showed one chunk twice
        // would invite two decisions on it.
        setChunks((current) => {
          const seen = new Set(current.map((c) => c.id));
          return [...current, ...found.filter((c) => !seen.has(c.id))];
        });
      }
      setError("");
      setState("ready");
    } catch (err: unknown) {
      if (mine !== generation.current) return;
      setError(err instanceof Error ? err.message : String(err));
      setState("error");
    } finally {
      busy.current = false;
    }
  }, [connection]);

  // A fresh domain list -- or an explicit reload -- restarts the walk from
  // empty. Everything loaded so far described a set that no longer applies.
  useEffect(() => {
    generation.current += 1;
    busy.current = false;
    walk.current = {
      domainIds: domainKey === "" ? [] : domainKey.split(" "),
      index: 0,
      cursor: "",
    };
    setChunks([]);
    setPagesRead(0);
    setExhausted(false);
    setError("");
    setState("idle");
    if (connection === null) return;
    if (domainKey === "") {
      // Nothing to walk. That is an ANSWER -- "nothing is awaiting review" --
      // and it has to be `ready` rather than `idle`, or the empty state would
      // read as a queue that never loaded.
      setExhausted(true);
      setState("ready");
      return;
    }
    void step();
    // `step` closes over `connection` alone and is stable in it; naming it in
    // the dependency list would restart the walk on every render that produced
    // a new closure.
  }, [connection, domainKey, attempt, step]);

  const applyDecision = useCallback((chunkId: string, status: string) => {
    setChunks((current) =>
      current.map((c) => (c.id === chunkId ? { ...c, validationStatus: status } : c)),
    );
  }, []);

  const loadMore = useCallback(() => void step(), [step]);
  const reload = useCallback(() => setAttempt((n) => n + 1), []);

  return { chunks, state, error, pagesRead, exhausted, loadMore, reload, applyDecision };
}
