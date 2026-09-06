import { useCallback, useEffect, useRef, useState } from "react";

// An ON-DEMAND read that says when it looked.
//
// ===========================================================================
// WHY THESE SURFACES ARE NOT LIVE, AND WHY THAT IS THE HONEST ANSWER
// ===========================================================================
// The Cluster and Stores surfaces read things the graph does not broadcast:
// the module inventory answers from the responding binary's own registries,
// `dataOrigins` is a virtual projection that is never persisted, and the
// connector health rows carry no routing rule. There is no `graph.node.*`
// event for any of them, so a `useLiveCollection` over one renders "Loading
// from the cluster" and then a list that never moves -- the exact failure
// the Logs app's own notes call out for `v1:observability:logLine`.
//
// So these read once, PRINT WHEN THEY LOOKED, and offer to look again. That
// is the Accounts ledger's rule (README, "the ledger is an on-demand read"),
// and its reasoning carries: a surface where half the bands move and half do
// not is worse than one where none do, because the reader cannot tell which
// kind of band they are looking at.
//
// EACH READING SETTLES ON ITS OWN. That is what the hook is for rather than
// a `Promise.all` in each section: a single combined await lets the one read
// that WILL be refused decide the state of the reads that succeeded, which
// is how a cluster owner's Modules list disappears because the data-origins
// call next to it came back empty.

export type ReadingState = "unread" | "reading" | "read" | "failed";

export interface Reading<T> {
  state: ReadingState;
  /** The value, once a read has landed. Null before that and after a failure. */
  value: T | null;
  /** The server's own sentence. Never a sentence this module invented. */
  error: string;
  /** When the value landed. Null until one has. */
  at: Date | null;
  /** Read again. Safe to call while a read is in flight -- it is ignored. */
  reread: () => void;
}

/**
 * Run `read` once on mount and on demand.
 *
 * `key` restarts the reading when it changes, the way `useLiveCollection`'s
 * key restarts a collection: it must encode everything `read` closes over,
 * or a section that switches store shows the previous store's numbers under
 * the new store's name.
 *
 * A null `read` is "not ready to ask yet" (no connection): the reading stays
 * `unread` rather than failing, because a missing connection is not a failed
 * read and must not render as one.
 */
export function useReading<T>(
  key: string,
  read: ((signal: AbortSignal) => Promise<T>) | null,
): Reading<T> {
  const [state, setState] = useState<ReadingState>("unread");
  const [value, setValue] = useState<T | null>(null);
  const [error, setError] = useState("");
  const [at, setAt] = useState<Date | null>(null);
  // A counter rather than a boolean: `reread` has to be able to ask again
  // after a read has already landed, and a boolean flag cannot express
  // "the same request, once more".
  const [attempt, setAttempt] = useState(0);
  // Guards the in-flight case so a double click does not open two reads.
  const running = useRef(false);

  const reread = useCallback(() => {
    if (running.current) return;
    setAttempt((n) => n + 1);
  }, []);

  useEffect(() => {
    if (read === null) {
      setState("unread");
      return;
    }
    const controller = new AbortController();
    let live = true;
    running.current = true;
    setState("reading");
    read(controller.signal)
      .then((next) => {
        if (!live) return;
        setValue(next);
        setError("");
        setAt(new Date());
        setState("read");
      })
      .catch((err: unknown) => {
        if (!live) return;
        // The server's own sentence is what a person can act on. A wrapper
        // sentence of ours ("could not load modules") replaces a message
        // naming the actual refusal with one naming the surface.
        setError(err instanceof Error ? err.message : String(err));
        setValue(null);
        setState("failed");
      })
      .finally(() => {
        running.current = false;
      });
    return () => {
      live = false;
      running.current = false;
      controller.abort();
    };
    // `key` and `attempt` are the whole dependency set by construction: key
    // encodes what `read` closes over, and attempt is the re-ask.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, attempt, read === null]);

  return { state, value, error, at, reread };
}
