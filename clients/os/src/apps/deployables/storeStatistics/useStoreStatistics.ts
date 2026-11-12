import { isTransportInterruption } from "@znasllc-io/memql-sdk-core/client";
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

import type { Period } from "../../../kit/statistics/period";
import { useOsConnection } from "../../../live/connection";
import { fetchStoreStatistics, type StoreStatistics } from "./read";

// One store's statistics for one period, read on demand and kept current, by
// the rules of `useSiteStatistics` (statistics/useSiteStatistics.ts), which
// says why at length:
//
//   - NOT A LIVE FEED. The figures are tables outside the graph; the page
//     reads once and `AutoRefresh` reads again on `statisticsRefreshMs`.
//   - THE SITE, THE STORE AND THE PERIOD ARE THE READ'S IDENTITY. A new one
//     drops what was held and aborts the read still out, so another store's
//     or another period's figures are never drawn under this one's name. A
//     refresh of the same identity keeps what it has.
//   - A FAILURE STANDS UNTIL A READ ANSWERS. While a retry is out the state
//     stays "error", so the engine's sentence stays on screen and no loading
//     shape is drawn over it.
//
// THE CLIENT ASKS; IT DOES NOT DECIDE. Whether the person may see this store's
// sales is the engine's to answer: a refusal comes back as its own sentence,
// shown verbatim, and is never replaced by a guess.

export interface StoreStatisticsReading {
  /** The read's answer, null before the first one and after an identity change. */
  value: StoreStatistics | null;
  /** "error" while the last answer was a failure, its retry included; "loading" only while nothing has failed. */
  state: "idle" | "loading" | "ready" | "error";
  /** The engine's own sentence for a refusal; empty for a dropped connection, which the shell reports. */
  error: string;
  /** Read again; one already out for the same identity is answered for both. */
  reload: () => Promise<void>;
}

export function useStoreStatistics({ siteId, storeId, period, enabled }: {
  siteId: string;
  /** The store of one of the site's destinations, as the site holds it. */
  storeId: string;
  period: Period;
  /** False reads nothing. */
  enabled: boolean;
}): StoreStatisticsReading {
  const connection = useOsConnection();
  const key = enabled && connection !== null && siteId !== "" && storeId !== "" ? JSON.stringify([siteId, storeId, period]) : "";

  const [value, setValue] = useState<StoreStatistics | null>(null);
  const [state, setState] = useState<StoreStatisticsReading["state"]>("idle");
  const [error, setError] = useState("");
  const out = useRef<{ key: string; controller: AbortController; done: Promise<void> } | null>(null);
  const latest = useRef({ key, connection, siteId, storeId, period });
  useLayoutEffect(() => {
    latest.current = { key, connection, siteId, storeId, period };
  });

  const read = useCallback((): Promise<void> => {
    const ask = latest.current;
    if (ask.key === "" || ask.connection === null) return Promise.resolve();
    if (out.current?.key === ask.key) return out.current.done;
    out.current?.controller.abort();
    const controller = new AbortController();
    setState((held) => (held === "error" ? "error" : "loading"));
    const done = fetchStoreStatistics(ask.connection.query, { siteId: ask.siteId, storeId: ask.storeId, period: ask.period }, controller.signal)
      .then((next) => {
        if (controller.signal.aborted) return;
        setValue(next);
        setError("");
        setState("ready");
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setError(isTransportInterruption(err) ? "" : err instanceof Error ? err.message : String(err));
        setState("error");
      })
      .finally(() => {
        if (out.current?.controller === controller) out.current = null;
      });
    out.current = { key: ask.key, controller, done };
    return done;
  }, []);

  useEffect(() => {
    setValue(null);
    setError("");
    if (key === "") {
      setState("idle");
      return;
    }
    setState("loading");
    void read();
    return () => {
      out.current?.controller.abort();
      out.current = null;
    };
  }, [key, read]);

  return { value, state, error, reload: read };
}
