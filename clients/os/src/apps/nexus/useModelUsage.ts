import { useCallback, useEffect, useState } from "react";
import { useOsConnection } from "../../live/connection";
import { modelUsageFromRows, type ModelUsage } from "./modelUsage";

export interface ModelUsageState {
  calls: ModelUsage[]; complete: boolean; error: string; loading: boolean;
  refresh: () => void; hasMore: boolean; loadMore: () => void;
}
/** Owner-scoped pages only. Background refresh reads one page; older receipts
 * are fetched one bounded page per explicit action and deduplicated by call. */
export function useModelUsage(runId: string, active: boolean): ModelUsageState {
  const connection = useOsConnection();
  const [nonce, update] = useState(0);
  const [request, setRequest] = useState({ runId, active, cursor: "" });
  // A run transition invalidates the old page cursor before any request is
  // issued. Finishing a paged live run must read the newest receipts again.
  const cursor = request.runId === runId && request.active === active ? request.cursor : "";
  const [state, setState] = useState({ runId, calls: [] as ModelUsage[], complete: false, next: "", error: "", loading: true });
  const refresh = useCallback(() => { setRequest({ runId, active, cursor: "" }); update(value => value + 1); }, [runId, active]);
  const loadMore = () => { if (state.runId === runId && state.next && !state.loading) setRequest({ runId, active, cursor: state.next }); };
  useEffect(() => {
    if (request.runId !== runId || request.active !== active) setRequest({ runId, active, cursor: "" });
    if (!runId || !connection) { setState({ runId, calls: [], complete: false, next: "", error: "", loading: false }); return; }
    const abort = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    setState(previous => previous.runId === runId ? { ...previous, complete: false, loading: true } : { runId, calls: [], complete: false, next: "", error: "", loading: true });
    void connection.query.workModelUsageForOwnerRun({ runId }, { signal: abort.signal, ...(cursor ? { cursor } : {}) }).then(result => {
      if (abort.signal.aborted) return;
      const incoming = modelUsageFromRows(result.rows(), runId), next = result.meta()?.cursor ?? "";
      setState(previous => {
        const held = cursor && previous.runId === runId ? previous.calls : [];
        const calls = [...new Map([...incoming, ...held].map(call => [call.id, call])).values()];
        return { runId, calls, complete: !next && !active, next, error: "", loading: false };
      });
    }).catch(error => {
      if (!abort.signal.aborted) setState(previous => previous.runId === runId ? { ...previous, complete: false, error: error instanceof Error ? error.message : String(error), loading: false } : previous);
    }).finally(() => { if (!abort.signal.aborted && active && !cursor) timer = setTimeout(refresh, 4000); });
    return () => { abort.abort(); if (timer) clearTimeout(timer); };
  }, [connection, runId, active, nonce, cursor, refresh]);
  const current = state.runId === runId ? state : { calls: [], complete: false, next: "", error: "", loading: true };
  return { ...current, complete: current.complete && !active, refresh, loadMore, hasMore: Boolean(current.next) };
}
