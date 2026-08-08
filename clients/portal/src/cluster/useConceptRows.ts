// One concept's rows: the keyset walk plus live updates, wired to the
// cluster.
//
// The state machines are NOT here -- they are src/concepts/rowWalk.ts
// (paging) and src/concepts/liveBand.ts (CDC), both pure and both unit-tested
// with no browser, no server and no React. This module is only the wiring: it
// carries out the fetch the walk asked for, opens the subscription, and tears
// both down. That split is deliberate; a paging invariant asserted through
// render() and waitFor() is asserted through three layers that can each fail
// for unrelated reasons.
//
// THE CONTROL FLOW IS ONE-WAY, and that is what makes it correct:
//
//   reducer decides "a page at cursor C is wanted"  (inFlight + requestId)
//        -> effect performs exactly that fetch
//        -> effect echoes requestId back on the settle
//        -> reducer accepts it only if it is still the request it asked for
//
// Nothing reads "current" walk state out of a ref at settle time. That shape
// -- the obvious one -- silently drops a good page whenever a state update
// has been scheduled but not yet committed.

import { useCallback, useEffect, useMemo, useReducer, useRef, useState } from "react";
import {
  browseConceptPage,
  getRowByConceptAndId,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import {
  EMPTY_LIVE_BAND,
  applyGraphEvent,
  type LiveBandState,
} from "../concepts/liveBand";
import {
  INITIAL_WALK,
  canRequest,
  walkReducer,
  type WalkState,
} from "../concepts/rowWalk";
import { useCluster } from "./ClusterProvider";

// 100 rather than the SDK's 200 default. This is an interactive pane, not a
// bulk export: a smaller page paints sooner, and "Load more" is one click.
// The walk is correct at any page size -- this is a latency choice.
export const ROW_PAGE_SIZE = 100;

export interface ConceptRowsState {
  walk: WalkState;
  live: LiveBandState;
  // Set when the CDC subscription could not be opened. Kept SEPARATE from the
  // walk's error, for the reason editors/vscode/src/webview/
  // conceptPanelState.ts spells out at length: an ordinary query succeeding
  // must not erase a "live updates are off" notice, or the pane looks live
  // moments after going deaf.
  liveDegraded: string;
  loadMore: () => void;
  retry: () => void;
  // Restart the walk from the first page and clear the live band. The honest
  // response to "rows changed since you loaded this", and the only exit from
  // a cursor-loop failure.
  reload: () => void;
}

export function useConceptRows(conceptId: string): ConceptRowsState {
  const { query, subscriptions } = useCluster();
  const [walk, dispatch] = useReducer(walkReducer, INITIAL_WALK);
  const [live, setLive] = useState<LiveBandState>(EMPTY_LIVE_BAND);
  const [liveDegraded, setLiveDegraded] = useState("");
  // Bumped by reload(). A counter separate from the walk's own generation
  // because it has to re-run the EFFECTS (reset, subscribe), and an effect
  // cannot depend on a value it also writes.
  const [epoch, setEpoch] = useState(0);

  // Every row id currently in the paged window, for the live band's
  // already-on-screen check.
  const pagedRowIds = useMemo(() => {
    const ids = new Set<string>();
    for (const row of walk.rows) {
      const id = row["id"];
      if (typeof id === "string") ids.add(id);
    }
    return ids;
  }, [walk.rows]);

  // Held in a ref, NOT passed as an effect dependency. The subscription must
  // survive paging: making the set a dependency would tear the socket
  // subscription down and re-open it on every "Load more", which is both
  // wasteful and a window during which live rows are silently missed. A ref
  // is the right tool here precisely because the handler wants the LATEST
  // committed value at event time, not the one captured when it subscribed.
  const pagedRowIdsRef = useRef(pagedRowIds);
  pagedRowIdsRef.current = pagedRowIds;

  // ---- reset ------------------------------------------------------------
  // On a concept change, a connection change, or an explicit reload.
  // Resetting on a CONNECTION change matters as much as on a concept change:
  // a reconnect re-mints cursors, so continuing a walk across one would hand
  // the new stream a token the old query minted.
  useEffect(() => {
    dispatch({ kind: "reset" });
    setLive(EMPTY_LIVE_BAND);
  }, [query, conceptId, epoch]);

  // ---- kick -------------------------------------------------------------
  // "idle" is the machine saying it wants a page and has none outstanding.
  // Both the first page after a reset and a resumed page after `retry` land
  // here, so there is exactly one place that starts a request.
  useEffect(() => {
    if (query === null || conceptId === "") return;
    if (walk.status !== "idle") return;
    dispatch({ kind: "requested", generation: walk.generation, cursor: walk.cursor });
  }, [query, conceptId, walk.status, walk.generation, walk.cursor]);

  // ---- fetch ------------------------------------------------------------
  // Carries out whatever the reducer asked for, and nothing else.
  const { inFlight, requestId, requestCursor, generation } = walk;
  useEffect(() => {
    if (!inFlight) return;
    if (query === null || conceptId === "") return;
    let live_ = true;

    void browseConceptPage(query, conceptId, {
      pageSize: ROW_PAGE_SIZE,
      ...(requestCursor ? { cursor: requestCursor } : {}),
    })
      .then((page) => {
        if (!live_) return;
        dispatch({
          kind: "arrived",
          generation,
          requestId,
          page: { rows: page.rows, nextCursor: page.nextCursor },
        });
      })
      .catch((err: unknown) => {
        if (!live_) return;
        dispatch({
          kind: "failed",
          generation,
          requestId,
          error: err instanceof Error ? err.message : String(err),
        });
      });

    return () => {
      live_ = false;
    };
  }, [inFlight, requestId, requestCursor, generation, query, conceptId]);

  // ---- live updates -----------------------------------------------------
  // Scoped to this concept and to the three graph verbs. The SERVER composes
  // the bus topic from them (memql#2460), so no topic string appears here.
  useEffect(() => {
    if (subscriptions === null || conceptId === "") {
      setLiveDegraded("");
      return;
    }
    let unsubscribe: (() => void) | null = null;
    try {
      unsubscribe = subscriptions.subscribeGraph(
        (event) => {
          setLive((current) =>
            applyGraphEvent(current, event, {
              conceptId,
              pagedRowIds: pagedRowIdsRef.current,
            }),
          );
        },
        { concept: conceptId, actions: ["created", "updated", "deleted"] },
      );
      setLiveDegraded("");
    } catch (err) {
      // A failed subscribe does NOT break the pane -- ordinary queries still
      // work on the same connection. It breaks the PROMISE that the pane is
      // live, and that has to be said out loud rather than inferred from rows
      // never appearing.
      setLiveDegraded(err instanceof Error ? err.message : String(err));
    }
    return () => {
      unsubscribe?.();
    };
  }, [subscriptions, conceptId, epoch]);

  const loadMore = useCallback(() => {
    if (!canRequest(walk)) return;
    dispatch({ kind: "requested", generation: walk.generation, cursor: walk.cursor });
  }, [walk]);

  const retry = useCallback(() => dispatch({ kind: "retry" }), []);
  const reload = useCallback(() => setEpoch((n) => n + 1), []);

  return { walk, live, liveDegraded, loadMore, retry, reload };
}

// useRowDetail resolves ONE row authoritatively.
//
// Deliberately a fresh read rather than a lookup in the paged rows the list
// already holds. The list's copy is as old as the page it arrived on, and the
// detail pane is precisely where an operator goes to check a current value.
// It is also what makes a live-band row -- whose only representation so far
// is a CDC event payload -- openable at all.
export interface RowDetailState {
  row: Row | null;
  loading: boolean;
  error: string;
  // True when the read succeeded and the cluster had no such row. Distinct
  // from `row === null` before the read lands, and from an error: "gone" and
  // "could not ask" are different facts and a deep link can produce either.
  missing: boolean;
}

const IDLE_DETAIL: RowDetailState = {
  row: null,
  loading: false,
  error: "",
  missing: false,
};

export function useRowDetail(conceptId: string, rowId: string): RowDetailState {
  const { query } = useCluster();
  const [state, setState] = useState<RowDetailState>(IDLE_DETAIL);

  useEffect(() => {
    if (query === null || conceptId === "" || rowId === "") {
      setState(IDLE_DETAIL);
      return;
    }
    let live = true;
    setState({ row: null, loading: true, error: "", missing: false });

    void getRowByConceptAndId(query, conceptId, rowId)
      .then((row) => {
        if (!live) return;
        setState({ row, loading: false, error: "", missing: row === null });
      })
      .catch((err: unknown) => {
        if (!live) return;
        setState({
          row: null,
          loading: false,
          error: err instanceof Error ? err.message : String(err),
          missing: false,
        });
      });

    return () => {
      live = false;
    };
  }, [query, conceptId, rowId]);

  return state;
}
