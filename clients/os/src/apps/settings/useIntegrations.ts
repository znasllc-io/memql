import { useCallback, useEffect, useRef, useState } from "react";

import { useOsConnection } from "../../live/connection";
import { readIntegrationsReport, type IntegrationsReport } from "./integrationsReport";

// The Integrations read (issue #4826 / program decision P6).
//
// Request/reply with an explicit Refresh, exactly like `useClusterFacts`'s
// registry projections and for the same reason: `integrationStatus` is a
// projection of ONE NODE's in-memory registry, not of rows anyone writes, so
// there is no graph event to subscribe to. Which replica answered is not
// knowable from here and the section says so.
//
// THE PROBE IS AN ACTION, AND THAT IS ENFORCED BY SHAPE RATHER THAN BY CARE.
// A live check dials a third party -- Entra's token endpoint, or an SMTP
// relay -- on the caller's say-so, and a probe on mount would reach out to a
// vendor every time somebody opened Settings. So the mounting effect reads
// with `probe: false` and CANNOT do otherwise: the flag is a literal inside
// it, not a dependency it could be re-run with. The live check is a separate
// imperative call with no effect behind it at all, so there is no render path
// that reaches it.

export interface IntegrationsFacts {
  report: IntegrationsReport | null;
  /** True while the configuration read is in flight. */
  loading: boolean;
  /** True while the LIVE CHECK is in flight. Separate, because they are
   *  separate acts and a spinner on the wrong control says the wrong thing. */
  checking: boolean;
  /** The engine's own refusal text when a read was declined. */
  error: string;
  /** Client clock, for the "read at" line when the reply carries no stamp. */
  fetchedAt: number | null;
  reload: () => void;
  check: () => void;
}

export function useIntegrations(): IntegrationsFacts {
  const connection = useOsConnection();
  const [report, setReport] = useState<IntegrationsReport | null>(null);
  const [loading, setLoading] = useState(false);
  const [checking, setChecking] = useState(false);
  const [error, setError] = useState("");
  const [fetchedAt, setFetchedAt] = useState<number | null>(null);
  // Refresh is an epoch counter, not a cache invalidation protocol: the
  // question "what does a node say right now" has no cache to invalidate.
  const [epoch, setEpoch] = useState(0);
  const reload = useCallback(() => setEpoch((n) => n + 1), []);

  // Set on unmount so an in-flight LIVE CHECK -- which has no effect cleanup
  // of its own, by design -- cannot write into a component that is gone.
  const gone = useRef(false);
  useEffect(() => {
    gone.current = false;
    return () => {
      gone.current = true;
    };
  }, []);

  useEffect(() => {
    if (connection === null) return;
    const controller = new AbortController();
    let stale = false;
    setLoading(true);
    setError("");
    void connection.query
      .integrationStatus({ probe: false }, { signal: controller.signal })
      .then((result) => {
        if (stale) return;
        setReport(readIntegrationsReport([...result.rows()]));
        setFetchedAt(Date.now());
      })
      .catch((err: unknown) => {
        if (stale) return;
        // A server-side refusal arrives as a rejected promise carrying the
        // engine's own words. It renders IN-SURFACE where the panel would be,
        // never as a toast and never rewritten.
        setReport(null);
        setError(messageOf(err));
      })
      .finally(() => {
        if (!stale) setLoading(false);
      });
    return () => {
      stale = true;
      controller.abort();
    };
  }, [connection, epoch]);

  const check = useCallback(() => {
    if (connection === null) {
      setError("Not connected to the cluster, so nothing was checked.");
      return;
    }
    setChecking(true);
    setError("");
    void connection.query
      .integrationStatus({ probe: true })
      .then((result) => {
        if (gone.current) return;
        // The probed reply is a WHOLE report carrying the verdict in each
        // integration's own detail, so it replaces rather than annotates.
        // Keeping a probe result beside a newer configuration read would let
        // the card show a verdict about credentials it is no longer showing.
        setReport(readIntegrationsReport([...result.rows()]));
        setFetchedAt(Date.now());
      })
      .catch((err: unknown) => {
        if (gone.current) return;
        setError(messageOf(err));
      })
      .finally(() => {
        if (!gone.current) setChecking(false);
      });
  }, [connection]);

  return { report, loading, checking, error, fetchedAt, reload, check };
}

function messageOf(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}
