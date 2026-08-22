import { useCallback, useEffect, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";

// Who is still outstanding on an invitation (memql#4271).
//
// The read gates ITSELF: pendingUserInvitations carries requiresOwnerOrAdmin as
// a top-level conjunct in its own DSL filter, so the engine empties the result
// for a caller below the floor whatever this code renders. The `enabled` flag
// is a courtesy -- it stops the portal issuing a read it knows will come back
// empty -- and never the authorization.
//
// Live, on the same epoch pattern the console's tiles use: an invitation issued
// or revoked a moment ago should not need a reload to disappear from this list.

export interface PendingInvitationsState {
  rows: Row[];
  loading: boolean;
  error: string;
  reload: () => void;
}

export function usePendingInvitations(enabled: boolean): PendingInvitationsState {
  const { query, subscriptions, status } = useCluster();
  const [rows, setRows] = useState<Row[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (query === null || !enabled) {
      setRows([]);
      setLoading(false);
      setError("");
      return;
    }
    let live = true;
    setLoading(true);
    setError("");

    let read: Promise<{ rawNodes: () => Row[] }>;
    try {
      read = query.pendingUserInvitations({});
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
      setLoading(false);
      return;
    }

    void read
      .then((result) => {
        if (!live) return;
        setRows(result.rawNodes());
        setLoading(false);
      })
      .catch((err: unknown) => {
        if (!live) return;
        setError(err instanceof Error ? err.message : String(err));
        setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, enabled, epoch]);

  useEffect(() => {
    if (!enabled || subscriptions === null || status !== "connected") return;
    try {
      return subscriptions.subscribeGraph(() => setEpoch((n) => n + 1), {
        concept: "v1:identity:invitation",
        actions: ["created", "updated"],
      });
    } catch {
      // A cluster whose subscription surface refuses is still usable here: the
      // list is correct, it just stops being live. Losing the whole read over
      // the live half would be worse.
      return;
    }
  }, [enabled, subscriptions, status]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);
  return { rows, loading, error, reload };
}
