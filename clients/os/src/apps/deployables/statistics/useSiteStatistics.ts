import { isTransportInterruption } from "@znasllc-io/memql-sdk-core/client";
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

import type { Period } from "../../../kit/statistics/period";
import { useOsConnection } from "../../../live/connection";
import { fetchSiteStatistics, type SiteStatistics } from "./read";

// One period of website statistics, read on demand and kept current.
//
// ===========================================================================
// NOT A LIVE FEED, AND THAT IS THE HONEST ANSWER
// ===========================================================================
// The statistics are rollup tables outside the graph: nothing publishes an
// event when a day's figures move, so a subscription would draw a page that
// never changes. The page reads once, and `AutoRefresh` (mounted beside the
// figures, so a hidden pane stops) reads again on the cadence below, and on
// the app's return. NOTHING CACHES IT: the engine answers a top-level builtin
// before its result cache is consulted (component/memql/engine.go), so every
// read reaches the rollup tables, and the cadence below is what bounds the
// load. Two pages asking the same question cost two reads.
//
// ===========================================================================
// WHAT A READ BELONGS TO
// ===========================================================================
// The deployables, the organizations and the period are the read's IDENTITY.
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

export interface StatisticsReading {
  /** The read's answer, null before the first one and after an identity change. */
  value: SiteStatistics | null;
  /** "error" while the last answer was a failure, its retry included; "loading" only while nothing has failed. */
  state: "idle" | "loading" | "ready" | "error";
  /** The server's own sentence for a refusal; empty for a dropped connection, which the shell reports. */
  error: string;
  /** Read again; one already out for the same identity is answered for both. */
  reload: () => Promise<void>;
}

/**
 * How often a page re-reads: five minutes for the last 24 hours, whose hours
 * fill as they pass, fifteen for a period of days or months, which a few more
 * minutes barely move (spec section 7).
 */
export function statisticsRefreshMs(period: Period): number {
  return period === "24h" ? 300_000 : 900_000;
}

export function useSiteStatistics({ siteIds, accountIds, period, enabled }: {
  /** The deployables to read; absent reads every one the caller may read. */
  siteIds?: readonly string[];
  /** With no `siteIds`, keeps the read to these organizations; `[]` is all of them. */
  accountIds?: readonly string[];
  period: Period;
  /** False reads nothing: a system-owned or draft deployable, a page not ready to ask. */
  enabled: boolean;
}): StatisticsReading {
  const connection = useOsConnection();
  const sites = siteIds === undefined ? undefined : [...siteIds];
  const accounts = accountIds === undefined ? undefined : [...accountIds];
  // THE IDENTITY, as one string, so a fresh array of the same ids is the same read.
  const key = enabled && connection !== null ? JSON.stringify([sites ?? null, accounts ?? null, period]) : "";

  const [value, setValue] = useState<SiteStatistics | null>(null);
  const [state, setState] = useState<StatisticsReading["state"]>("idle");
  const [error, setError] = useState("");
  // The read that is out, with the identity it was asked for.
  const out = useRef<{ key: string; controller: AbortController; done: Promise<void> } | null>(null);
  const latest = useRef({ key, connection, sites, accounts, period });
  useLayoutEffect(() => {
    latest.current = { key, connection, sites, accounts, period };
  });

  const read = useCallback((): Promise<void> => {
    const ask = latest.current;
    if (ask.key === "" || ask.connection === null) return Promise.resolve();
    if (out.current?.key === ask.key) return out.current.done;
    out.current?.controller.abort();
    const controller = new AbortController();
    // A retry of a failed read leaves the failure standing until it answers.
    setState((held) => (held === "error" ? "error" : "loading"));
    const done = fetchSiteStatistics(ask.connection.query, { siteIds: ask.sites, accountIds: ask.accounts, period: ask.period }, controller.signal)
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
    // A new question: whatever failed before was about another one.
    setState("loading");
    void read();
    return () => {
      out.current?.controller.abort();
      out.current = null;
    };
  }, [key, read]);

  return { value, state, error, reload: read };
}
