import { isTransportInterruption, type QueryClient } from "@znasllc-io/memql-sdk-core/client";
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

import { useOsConnection } from "../../../live/connection";

// THE ONE READ every statistics page of this app makes: on demand, kept
// current by the page's `AutoRefresh`, and governed by the rules below. The
// website statistics (`useSiteStatistics`) and a store's (`useStoreStatistics`)
// are thin wrappers that say only what they read and what the read's identity
// is, so a rule fixed here holds for both.
//
// ===========================================================================
// NOT A LIVE FEED, AND THAT IS THE HONEST ANSWER
// ===========================================================================
// The statistics are rollup tables outside the graph: nothing publishes an
// event when a day's figures move, so a subscription would draw a page that
// never changes. The page reads once, and `AutoRefresh` (mounted beside the
// figures, so a hidden pane stops) reads again on `statisticsRefreshMs`'s cadence
// (useSiteStatistics.ts), and on the app's return. NOTHING CACHES IT: the engine answers a top-level builtin
// before its result cache is consulted (component/memql/engine.go), so every
// read reaches the rollup tables, and that cadence is what bounds the load. Two pages asking the same question cost two reads.
//
// ===========================================================================
// WHAT A READ BELONGS TO
// ===========================================================================
// What a wrapper names as its identity (the deployables, the organizations
// and the period; a site, a store and the period) is the read's IDENTITY.
// A new identity drops the figures held for the old one and starts again --
// the old period's figures under the new period's name would be a claim
// nobody made -- and the read still out for the old one is ABORTED, so it can
// never land over the new one. A refresh of the same identity keeps the
// figures it has while it reads (DESIGN.md, "Preserve loaded rows during
// refresh"), and a refresh that fails keeps them too, beside the failure.
//
// A FAILURE STANDS UNTIL A READ ANSWERS. A retry asks the same question again,
// so while it is out the state stays "error" rather than turning to
// "loading": the page keeps the refusal on screen, and draws no loading shapes
// over it (DESIGN.md: a skeleton must never conceal an error). Only a new
// identity starts over, because the failure was an answer to another question.
// Regression: test/deployables/statistics.test.tsx, "keeps a refusal standing
// while its retry is out".


export interface StatisticsRead<T> {
  /** The read's answer, null before the first one and after an identity change. */
  value: T | null;
  /** "error" while the last answer was a failure, its retry included; "loading" only while nothing has failed. */
  state: "idle" | "loading" | "ready" | "error";
  /** The server's own sentence for a refusal; empty for a dropped connection, which the shell reports. */
  error: string;
  /** A read is out, a retry of a failed one included: a page offers no second retry beside it. */
  inFlight: boolean;
  /** Read again; one already out for the same identity is answered for both. */
  reload: () => Promise<void>;
}

/**
 * One statistics read.
 *
 * `identity` is everything the answer depends on, compared as JSON, so a
 * fresh array of the same ids is the same read. `enabled` false reads
 * nothing (state "idle"); so does a window with no connection. `fetch` is
 * called with the connection's query client and the read's abort signal, and
 * the latest one is used, so it need not be stable.
 */
export function useStatisticsRead<T>({ identity, enabled, fetch }: {
  identity: unknown;
  enabled: boolean;
  fetch: (query: QueryClient, signal: AbortSignal) => Promise<T | null>;
}): StatisticsRead<T> {
  const connection = useOsConnection();
  const key = enabled && connection !== null ? JSON.stringify(identity) : "";

  const [value, setValue] = useState<T | null>(null);
  const [state, setState] = useState<StatisticsRead<T>["state"]>("idle");
  const [error, setError] = useState("");
  const [inFlight, setInFlight] = useState(false);
  // The read that is out, with the identity it was asked for.
  const out = useRef<{ key: string; controller: AbortController; done: Promise<void> } | null>(null);
  const latest = useRef({ key, connection, fetch });
  useLayoutEffect(() => {
    latest.current = { key, connection, fetch };
  });

  const read = useCallback((): Promise<void> => {
    const ask = latest.current;
    if (ask.key === "" || ask.connection === null) return Promise.resolve();
    if (out.current?.key === ask.key) return out.current.done;
    out.current?.controller.abort();
    const controller = new AbortController();
    // A retry of a failed read leaves the failure standing until it answers.
    setState((held) => (held === "error" ? "error" : "loading"));
    setInFlight(true);
    const done = ask.fetch(ask.connection.query, controller.signal)
      .then((next) => {
        if (controller.signal.aborted) return;
        setValue(next);
        setError("");
        setState("ready");
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        // VERBATIM: a read the cluster refused says why in its own words. A
        // dropped connection is the shell's to report (ConnectionNotice), so it
        // leaves no sentence of its own here.
        setError(isTransportInterruption(err) ? "" : err instanceof Error ? err.message : String(err));
        setState("error");
      })
      .finally(() => {
        if (out.current?.controller !== controller) return;
        out.current = null;
        setInFlight(false);
      });
    out.current = { key: ask.key, controller, done };
    return done;
  }, []);

  useEffect(() => {
    setValue(null);
    setError("");
    if (key === "") {
      setState("idle");
      setInFlight(false);
      return;
    }
    // A new question: whatever failed before was about another one.
    setState("loading");
    void read();
    return () => {
      out.current?.controller.abort();
      out.current = null;
    };
  }, [key, read]);

  return { value, state, error, inFlight, reload: read };
}
