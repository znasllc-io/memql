import { useCallback, useEffect, useRef, useState } from "react";
import { browseConceptPage, getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";

// A concept's rows: a keyset walk, and a band saying what arrived while you
// were reading.
//
// ===========================================================================
// WHY THE ARRIVALS ARE A BAND AND NOT SPLICED IN
// ===========================================================================
// `browseConceptPage` walks `sort(paginate(concept==X, N), "createdAt",
// "asc")` -- oldest first, with a cursor bound to that ordering. So a row
// created while somebody is reading belongs at the END of the total order,
// after pages this walk has very likely not reached yet. Splicing it into
// what is on screen would draw it among rows it does not belong between,
// and the next `loadMore` would fetch it again.
//
// So new rows are COUNTED, not inserted, and the person is offered a
// reload. That is the portal's decision and it holds for the same reason.
//
// It is also why this is not `kit/LiveList`. `LiveList` takes a
// `LiveListSource` -- a `LiveCollection`, whose whole model is one
// authoritative fold that new events are applied INTO. A paged walk is not
// one, and dressing it as one would put the ring on a row whose position is
// wrong.
//
// ===========================================================================
// AN ID-ONLY EVENT IS RE-READ, AND A REFUSAL IS SILENCE
// ===========================================================================
// A concept on the `granted` tier fans out id-only (`payloadOmitted`),
// because the predicate needs a join the fan-out cannot do against one row.
// Counting those blind would tell somebody "4 new rows" about rows they may
// not read. So each one is re-read through the ordinary authorized path, and
// a refusal drops it -- the SDK's own instruction, and the only reading that
// keeps the number honest.

export type WalkStatus = "loading" | "more" | "exhausted" | "failed";

export interface ConceptRowsWalk {
  rows: Row[];
  status: WalkStatus;
  /** The server's own sentence when the walk failed. */
  error: string;
  /** Rows created since this walk started that it has not loaded. */
  arrivedCount: number;
  /** Loaded rows the cluster has changed or removed since. */
  changedCount: number;
  loadMore: () => void;
  /** Start the walk again from the first page, clearing the band. */
  reload: () => void;
}

export function useConceptRows(conceptId: string, pageSize: number): ConceptRowsWalk {
  const connection = useOsConnection();
  const query = connection?.query ?? null;
  const subscriptions = connection?.subscriptions ?? null;

  const [rows, setRows] = useState<Row[]>([]);
  const [status, setStatus] = useState<WalkStatus>("loading");
  const [error, setError] = useState("");
  const [cursor, setCursor] = useState("");
  const [arrivedCount, setArrivedCount] = useState(0);
  const [changedCount, setChangedCount] = useState(0);
  const [attempt, setAttempt] = useState(0);
  // What the walk holds, readable from the subscription handler without
  // making the handler depend on -- and therefore re-register on -- every
  // page that lands. A registration effect that keys on live values
  // re-registers constantly and loses events between teardown and setup.
  const loadedIds = useRef<Set<string>>(new Set());
  const arrivedIds = useRef<Set<string>>(new Set());

  const reload = useCallback(() => {
    loadedIds.current = new Set();
    arrivedIds.current = new Set();
    setRows([]);
    setCursor("");
    setArrivedCount(0);
    setChangedCount(0);
    setError("");
    setStatus("loading");
    setAttempt((n) => n + 1);
  }, []);

  // Restart when the concept or the page size CHANGES -- both change what the
  // walk is, and continuing a cursor across either would page one concept
  // with another's continuation.
  //
  // NOT ON MOUNT, and the ref is what makes that true. A browser found this
  // (epic memql#5009): on the first render this effect and the walk effect
  // below both ran, the reload bumped `attempt`, and the walk ran a SECOND
  // time and APPENDED its page to the first one's -- three rows rendered as
  // six, under a footer confidently reporting "All 6 readable rows loaded".
  //
  // The suite could not see it. Every test renders without StrictMode, where
  // the two effects settle in an order that happens to hide it; the shell
  // mounts under StrictMode, where it does not. So the guard is a ref rather
  // than a dependency-list change: what has to be expressed is "the walk this
  // hook already started IS the walk for these arguments", and only state
  // that survives a re-render can say so.
  const walking = useRef("");
  useEffect(() => {
    const key = `${conceptId}\u0000${pageSize}`;
    if (walking.current === key) return;
    // The first run adopts the walk the mount already started rather than
    // restarting it -- reloading here would be the duplication above.
    const first = walking.current === "";
    walking.current = key;
    if (!first) reload();
  }, [conceptId, pageSize, reload]);

  // ---- the walk --------------------------------------------------------

  const [pageRequest, setPageRequest] = useState(0);
  const loadMore = useCallback(() => {
    setStatus((current) => (current === "loading" ? current : "loading"));
    setPageRequest((n) => n + 1);
  }, []);

  useEffect(() => {
    if (query === null || conceptId === "") return;
    const controller = new AbortController();
    let live = true;
    setStatus("loading");
    browseConceptPage(query, conceptId, {
      pageSize,
      ...(cursor === "" ? {} : { cursor }),
      signal: controller.signal,
    })
      .then((page) => {
        if (!live) return;
        for (const row of page.rows) {
          const id = rowIdOf(row);
          if (id !== "") {
            loadedIds.current.add(id);
            // A row the band was counting has now been walked to. It is no
            // longer news, and leaving it counted would keep offering a
            // reload that changes nothing.
            if (arrivedIds.current.delete(id)) {
              setArrivedCount(arrivedIds.current.size);
            }
          }
        }
        setRows((current) => [...current, ...page.rows]);
        setCursor(page.nextCursor);
        setStatus(page.nextCursor === "" ? "exhausted" : "more");
      })
      .catch((err: unknown) => {
        if (!live) return;
        if (controller.signal.aborted) return;
        setError(err instanceof Error ? err.message : String(err));
        setStatus("failed");
      });
    return () => {
      live = false;
      controller.abort();
    };
    // `cursor` is deliberately NOT a dependency: it is set BY this effect,
    // and depending on it would run the next page the moment one lands,
    // walking the whole concept in one go. `pageRequest` is the ask.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [query, conceptId, pageSize, attempt, pageRequest]);

  // ---- the band --------------------------------------------------------

  useEffect(() => {
    if (subscriptions === null || conceptId === "" || query === null) return;
    let live = true;

    const unregister = subscriptions.subscribeGraph(
      (event) => {
        if (!live) return;
        const payload = event.payload ?? {};
        const id = typeof payload["id"] === "string" ? payload["id"] : "";
        if (id === "") return;

        const record = () => {
          if (loadedIds.current.has(id)) {
            setChangedCount((n) => n + 1);
            return;
          }
          if (arrivedIds.current.has(id)) return;
          arrivedIds.current.add(id);
          setArrivedCount(arrivedIds.current.size);
        };

        if (!event.payloadOmitted) {
          record();
          return;
        }
        // Id-only: re-read under this caller's own authority. A refusal
        // means the row was never theirs to be told about, so it is dropped
        // rather than counted.
        getRowByConceptAndId(query, conceptId, id)
          .then((row) => {
            if (!live || row === null) return;
            record();
          })
          .catch(() => {
            // Silence is the correct answer here, and the only one.
          });
      },
      { concept: conceptId },
    );

    return () => {
      live = false;
      unregister();
    };
    // The handler reads its live state through refs, so this registers once
    // per (connection, concept) and stays registered -- a registration
    // effect that keyed on the loaded rows would tear down and re-register
    // on every page and lose whatever arrived in between.
  }, [subscriptions, query, conceptId, attempt]);

  return { rows, status, error, arrivedCount, changedCount, loadMore, reload };
}

/**
 * A row's id, from either shape.
 *
 * `browseConceptPage` returns RAW nodes, which carry the intrinsics nested;
 * a shaped read flattens them to the top level. Reading only one spelling
 * makes every row look id-less, which silently disables the band's dedupe.
 */
export function rowIdOf(row: Row): string {
  const direct = row["id"];
  if (typeof direct === "string" && direct !== "") return direct;
  const nested = row["node"];
  if (nested !== null && typeof nested === "object") {
    const inner = (nested as Record<string, unknown>)["id"];
    if (typeof inner === "string") return inner;
  }
  return "";
}
