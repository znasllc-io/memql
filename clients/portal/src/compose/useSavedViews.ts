import { useCallback, useEffect, useMemo, useState } from "react";
import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";
import { useLive } from "../cluster/useLive";
import { parseSavedView, parseSavedViews, savedViewArgs, type SavedView, type SavedViewInput } from "./savedViews";

const SAVED_VIEW_CONCEPT = "v1:portalviews:view";

// A view belongs on the rail only while it is active. The read says so
// server-side (`status: "active"`); this says the same about an arriving
// event, which is what makes an archive disappear rather than fold back in.
function isActiveView(row: Row): boolean {
  const payload = (row as { payload?: unknown }).payload;
  const source =
    payload && typeof payload === "object" && !Array.isArray(payload)
      ? (payload as Record<string, unknown>)
      : (row as Record<string, unknown>);
  const status = String(source["status"] ?? "");
  return status === "" || status === "active";
}

// Reading and writing composed views against the cluster.
//
// AUTHORIZATION IS NOT HERE, and its absence is the design rather than an
// oversight. `v1:portalviews:view` declares @rowAuthz(owner="ownerUserId") and
// both queries filter ownerUserId==actor.userId, so the row set this receives
// is already the caller's own and a write against somebody else's view is
// refused by the engine's write guard. A client-side owner check would imply
// the isolation lives in the browser, which is the belief this codebase is
// most careful not to encourage.

export interface SavedViewsState {
  views: SavedView[];
  loading: boolean;
  error: string;
  refresh: () => void;
}

export function useSavedViews(): SavedViewsState {
  const { query } = useCluster();

  // LIVE THROUGH THE STORE (memql#4539, carrying memql#4264). The rail lists
  // these, and a view composed a moment ago appearing only after a reload is
  // the kind of staleness that makes an operator doubt the save worked. It
  // used to bump an epoch and re-run the read on every CDC event; the rows
  // fold now, and an ARCHIVED view leaves the list because the read's own
  // `status: "active"` narrowing is re-applied to every folded row.
  //
  // A read that throws SYNCHRONOUSLY -- a client without the generated method,
  // an older engine -- is caught rather than left to propagate, because this
  // hook is on the NAV RAIL's path (memql#4264): losing the rail because a
  // saved-view list could not be read is wildly out of proportion, and an
  // empty Custom section is the right degradation.
  const live = useLive<Row>(query === null ? null : "compose:savedViews", () => ({
    concept: SAVED_VIEW_CONCEPT,
    actions: ["created", "updated", "deleted"],
    paged: false,
    seed: async (_cursor, signal) => {
      if (query === null) return { rows: [], nextCursor: "" };
      const result = await query.composedViews({ status: "active" }, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      if (query === null) return null;
      return getRowByConceptAndId(query, SAVED_VIEW_CONCEPT, rowId, { signal });
    },
    inScope: isActiveView,
  }));

  const views = useMemo(() => parseSavedViews(live.rows), [live.rows]);

  return {
    views,
    loading: query !== null && live.state === "seeding",
    error: live.error,
    refresh: live.reload,
  };
}

export interface SavedViewState {
  view: SavedView | null;
  loading: boolean;
  error: string;
  // True when the read succeeded and there was no such row. Distinct from an
  // error: "you have no view with that id" and "the read failed" want
  // different words, and a stale bookmark produces the first.
  missing: boolean;
}

export function useSavedView(viewId: string): SavedViewState {
  const { query } = useCluster();
  const [state, setState] = useState<SavedViewState>({
    view: null,
    loading: false,
    error: "",
    missing: false,
  });

  useEffect(() => {
    if (query === null || viewId === "") {
      setState({ view: null, loading: false, error: "", missing: false });
      return;
    }
    let live = true;
    setState({ view: null, loading: true, error: "", missing: false });

    void query
      .composedViewById({ viewId })
      .then((result) => {
        if (!live) return;
        const first = result.rows()[0];
        const view = first === undefined ? undefined : parseSavedView(first);
        setState({
          view: view ?? null,
          loading: false,
          error: "",
          missing: view === undefined,
        });
      })
      .catch((err: unknown) => {
        if (!live) return;
        setState({
          view: null,
          loading: false,
          error: err instanceof Error ? err.message : String(err),
          missing: false,
        });
      });

    return () => {
      live = false;
    };
  }, [query, viewId]);

  return state;
}

export interface SaveViewState {
  saving: boolean;
  error: string;
  // save resolves with the view's id on success and rejects on failure, so a
  // caller can navigate to the saved view without a second read.
  save: (input: SavedViewInput, mode: "create" | "update") => Promise<string>;
  archive: (viewId: string) => Promise<void>;
}

export function useSaveView(): SaveViewState {
  const { query } = useCluster();
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  const save = useCallback(
    async (input: SavedViewInput, mode: "create" | "update") => {
      if (query === null) throw new Error("Not connected to a cluster.");
      setSaving(true);
      setError("");
      try {
        const args = savedViewArgs(input);
        await (mode === "create" ? query.createComposedView(args) : query.updateComposedView(args));
        return input.viewId;
      } catch (err: unknown) {
        const message = err instanceof Error ? err.message : String(err);
        setError(message);
        throw err;
      } finally {
        setSaving(false);
      }
    },
    [query],
  );

  const archive = useCallback(
    async (viewId: string) => {
      if (query === null) throw new Error("Not connected to a cluster.");
      setSaving(true);
      setError("");
      try {
        await query.archiveComposedView({ viewId });
      } catch (err: unknown) {
        const message = err instanceof Error ? err.message : String(err);
        setError(message);
        throw err;
      } finally {
        setSaving(false);
      }
    },
    [query],
  );

  return { saving, error, save, archive };
}
