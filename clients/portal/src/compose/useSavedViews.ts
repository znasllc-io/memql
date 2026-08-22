import { useCallback, useEffect, useState } from "react";

import { useCluster } from "../cluster/ClusterProvider";
import { parseSavedView, parseSavedViews, savedViewArgs, type SavedView, type SavedViewInput } from "./savedViews";

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
  const { query, subscriptions, status } = useCluster();
  const [views, setViews] = useState<SavedView[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (query === null) {
      setViews([]);
      setLoading(false);
      setError("");
      return;
    }
    let live = true;
    setLoading(true);
    setError("");

    // Wrapped, because this hook is now on the NAV RAIL's path (memql#4264).
    // A read that throws SYNCHRONOUSLY -- a client without the generated
    // method, an older engine -- would otherwise take the whole shell down
    // with it, and losing the rail because a saved-view list could not be read
    // is wildly out of proportion. An empty Custom section is the right
    // degradation.
    let read: Promise<unknown>;
    try {
      read = query.composedViews({ status: "active" });
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
      setLoading(false);
      return;
    }

    void (read as ReturnType<typeof query.composedViews>)
      .then((result) => {
        if (!live) return;
        setViews(parseSavedViews(result.rows()));
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
  }, [query, epoch]);

  // LIVE (memql#4264). The rail lists these, and a view composed a moment ago
  // appearing only after a reload is the kind of staleness that makes an
  // operator doubt the save worked. Same shape as the console's tiles: a CDC
  // arrival bumps an epoch and the bounded read runs again -- no poll, and no
  // splicing of a partial row into a list the read owns.
  useEffect(() => {
    if (subscriptions === null || status !== "connected") return;
    try {
      return subscriptions.subscribeGraph(() => setEpoch((n) => n + 1), {
        concept: "v1:portalviews:view",
        actions: ["created", "updated", "deleted"],
      });
    } catch {
      // A cluster whose subscription surface refuses is still perfectly usable
      // here -- the list is correct, it just stops being live. Failing the
      // whole hook over the live half would be worse than losing it.
      return;
    }
  }, [subscriptions, status]);

  const refresh = useCallback(() => setEpoch((n) => n + 1), []);
  return { views, loading, error, refresh };
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
