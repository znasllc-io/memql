// The React binding for the SDK's LiveCollection (memql#4538).
//
// IT LIVES HERE, NOT IN sdk/ts, AND THAT IS THE RECORDED CHOICE. The SDK core
// is framework-free by design -- product SPAs, site surfaces and Node tools
// consume it without React -- so a hook in it would make every one of those
// consumers depend on React to get a store. The machine is the reusable part;
// this file is the adapter, and a second client writes its own in whatever
// framework it has.
//
// The store OUTLIVES the components that mount against it. It is owned by
// ClusterProvider and keyed on the connection inside the SDK, so navigating
// away and back reuses the live rows and issues NO new read -- which is the
// whole fix for "every page open refetches everything". That is why these
// hooks only retain and release: owning anything here would tie the store's
// life to a component's, and put the refetch straight back.

import { useCallback, useEffect, useRef, useState } from "react";
import type {
  LiveCollection,
  LiveCollectionSpec,
  LiveHandle,
  LiveSnapshot,
  LiveState,
  LiveValueSnapshot,
  Row,
} from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "./ClusterProvider";
import { bumpActivity } from "./activity";

// EMPTY is what a surface renders before there is a connection to read over.
// "disconnected" rather than "seeding": nothing is in flight, and a spinner
// over a connection that does not exist is the console lying about progress.
const EMPTY: LiveSnapshot<never> = { rows: [], state: "disconnected", error: "", version: 0 };
const EMPTY_VALUE: LiveValueSnapshot<never> = {
  value: null,
  state: "disconnected",
  error: "",
  version: 0,
};

export interface UseLiveResult<T> {
  rows: T[];
  state: LiveState;
  error: string;
  // Re-run the read. The honest answer to "this looks stale", and the same
  // mechanism a gap and a reconnect already use.
  reload: () => void;
}

/**
 * Subscribe a component to a shared live collection.
 *
 * `key` is the collection's IDENTITY -- the query name plus its arguments --
 * and two components passing the same key share one subscription, one seed and
 * one set of rows. Pass null to opt out (no connection yet, a route parameter
 * still empty); the hook must still be CALLED, because hook order cannot vary
 * between renders.
 *
 * `spec` is read once per (store, key). It is taken as a factory so a caller
 * can close over props without re-creating the collection on every render.
 */
export function useLive<T = Row>(
  key: string | null,
  spec: () => LiveCollectionSpec<T>,
): UseLiveResult<T> {
  const { store, status } = useCluster();
  const specRef = useRef(spec);
  specRef.current = spec;

  const handleRef = useRef<LiveHandle<LiveCollection<T>> | null>(null);
  const [snapshot, setSnapshot] = useState<LiveSnapshot<T>>(EMPTY as LiveSnapshot<T>);

  // RETAIN INSIDE THE EFFECT, never during render. React 19's StrictMode
  // renders twice before committing, and a retain taken during render would
  // never be balanced by the release in this cleanup -- the collection would
  // outlive every consumer, subscription and all.
  //
  // The double MOUNT is harmless by the same mechanism that makes navigation
  // free: the release lingers, so the second retain reuses the same
  // collection and issues no new read.
  useEffect(() => {
    if (store === null || key === null) {
      handleRef.current = null;
      setSnapshot(EMPTY as LiveSnapshot<T>);
      return;
    }
    const handle = store.collection<T>(key, specRef.current());
    handleRef.current = handle;
    setSnapshot(handle.value.snapshot);
    const off = handle.value.subscribe(() => {
      // The rail's activity mark keeps ticking off collection events; the
      // stream-activity singleton is unchanged by the store.
      bumpActivity();
      setSnapshot(handle.value.snapshot);
    });
    return () => {
      off();
      handle.release();
      if (handleRef.current === handle) handleRef.current = null;
    };
  }, [store, key]);

  const reload = useCallback(() => handleRef.current?.value.reseed(), []);

  return {
    rows: snapshot.rows,
    // A dead connection OUTRANKS the collection's own view. The store hears a
    // drop through its host, but a surface mounted while already disconnected
    // has a collection that never got to be live at all -- and rendering its
    // "seeding" would be a spinner over a socket nobody is holding.
    state: status === "connected" ? snapshot.state : "disconnected",
    error: snapshot.error,
    reload,
  };
}

export interface UseLiveValueResult<T> {
  value: T | null;
  state: LiveState;
  error: string;
  reload: () => void;
}

/**
 * The single-read counterpart: ONE shared, in-flight-deduped answer per
 * connection, however many components ask for it.
 *
 * `read` is captured per (store, key) through a ref, so a caller may pass an
 * inline closure without re-reading on every render.
 */
export function useLiveValue<T>(
  key: string | null,
  read: (signal: AbortSignal) => Promise<T | null>,
): UseLiveValueResult<T> {
  const { store, status } = useCluster();
  const readRef = useRef(read);
  readRef.current = read;

  const handleRef = useRef<{ release: () => void } | null>(null);
  const [snapshot, setSnapshot] = useState<LiveValueSnapshot<T>>(
    EMPTY_VALUE as LiveValueSnapshot<T>,
  );

  useEffect(() => {
    if (store === null || key === null) {
      handleRef.current = null;
      setSnapshot(EMPTY_VALUE as LiveValueSnapshot<T>);
      return;
    }
    const handle = store.value<T>(key, (signal) => readRef.current(signal));
    handleRef.current = handle;
    setSnapshot(handle.value.snapshot);
    const off = handle.value.subscribe(() => setSnapshot(handle.value.snapshot));
    return () => {
      off();
      handle.release();
      if (handleRef.current === handle) handleRef.current = null;
    };
  }, [store, key]);

  const reload = useCallback(() => {
    const handle = handleRef.current as { value?: { refresh: () => void } } | null;
    handle?.value?.refresh();
  }, []);

  return {
    value: snapshot.value,
    state: status === "connected" ? snapshot.state : "disconnected",
    error: snapshot.error,
    reload,
  };
}
