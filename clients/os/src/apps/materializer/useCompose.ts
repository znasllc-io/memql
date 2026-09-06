import { useCallback, useEffect, useState } from "react";
import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import { COMPOSITION_CONCEPT, RECIPE_CONCEPT, TEMPLATE_CONCEPT } from "./concepts";

// The Materializer's feeds, and the one read that is deliberately not a
// feed.
//
// ===========================================================================
// THREE FEEDS AT THE APP ROOT, ONE PER CONCEPT
// ===========================================================================
// Compositions, templates and recipes are retained ONCE, at the app root,
// and passed down. `useLiveCollection` constructs a collection per
// COMPONENT -- it memoises on `[connection, key]` inside the hook and does
// not call the SDK's shared registry -- so a second `useCompositions()`
// inside the Materialized list would open a second subscription and run a
// second seed over the same concept, and the two would then be free to
// disagree about what the cluster holds. That is the Deployables
// map-and-list failure.
//
// The rule is per CONCEPT, not per app, so three feeds is not three copies
// of one thing.
//
// ===========================================================================
// THE SEED CARRIES NO ARCHIVE FILTER, AND THAT IS THE POINT
// ===========================================================================
// `compositions` deliberately declares no `archived != true` conjunct
// (dsl/compose/queries.memql says so). A read that carried one could only
// ever back a show-archived preference that revealed the rows which
// flipped WHILE THE WINDOW WAS OPEN -- the Files app's own lesson. So one
// paged seed holds the complete truth and the archived / not split stays a
// client-side fold over it.
//
// ===========================================================================
// THE COMPOSABLE CONCEPTS ARE A READ, NOT A FEED
// ===========================================================================
// They come from the concept REGISTRY rather than from rows, so there is
// no `graph.node.*` event for a subscription to receive -- a
// `useLiveCollection` over them would render "Loading from the cluster"
// and then a list that silently never moved. It is read once when the
// Sources column opens and re-read on demand, which is honest: the set
// changes when a DSL bundle is redeployed, not while somebody is
// composing.

/** Every composition this caller can read, newest first. */
export function useCompositions(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("compose:compositions", (connection) => ({
    concept: COMPOSITION_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.compositions({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, COMPOSITION_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}

/** Every template binding this caller owns. */
export function useTemplates(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("compose:templates", (connection) => ({
    concept: TEMPLATE_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.composeTemplates({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, TEMPLATE_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}

/** Every recipe this caller owns. */
export function useRecipes(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("compose:recipes", (connection) => ({
    concept: RECIPE_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.composeRecipes({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, RECIPE_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}

// ---------------------------------------------------------------------------
// The composable concepts: an on-demand read that says when it looked
// ---------------------------------------------------------------------------

export interface ComposableConcept {
  id: string;
  as: string;
  fields: string[];
  list: string;
  description: string;
  marked: boolean;
}

export interface ComposablesState {
  concepts: ComposableConcept[];
  loading: boolean;
  /** The server's own sentence, verbatim. "" when the last read worked. */
  error: string;
  /** When this window last looked. Empty before the first read settles. */
  readAt: string;
  /**
   * Whether the node answering could see the concept registry at all.
   *
   * "NOTHING IS MARKED" AND "THIS NODE CANNOT SEE THE REGISTRY" LOOK
   * IDENTICAL from an empty list, and only one of them is something an
   * operator can fix -- so the engine reports which and the surface says
   * so rather than rendering a bare empty state over both.
   */
  registryAvailable: boolean;
  reread: () => void;
}

export function useComposableConcepts(includeUnmarked: boolean): ComposablesState {
  const connection = useOsConnection();
  const [concepts, setConcepts] = useState<ComposableConcept[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [readAt, setReadAt] = useState("");
  const [registryAvailable, setRegistryAvailable] = useState(true);
  const [nonce, setNonce] = useState(0);

  const reread = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    const query = connection?.query ?? null;
    if (query === null) return;
    let cancelled = false;
    setLoading(true);
    setError("");
    void (async () => {
      try {
        const result = await query.composableConcepts(
          includeUnmarked ? { includeUnmarked: true } : {},
        );
        if (cancelled) return;
        const reply = result.rows()[0] ?? null;
        setConcepts(conceptsFromReply(reply));
        setRegistryAvailable(reply?.["registryAvailable"] !== false);
        setReadAt(new Date().toISOString());
      } catch (err) {
        if (cancelled) return;
        setError(err instanceof Error ? err.message : String(err));
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
    // `includeUnmarked` is a DIFFERENT READING and re-baselines, which is
    // what keying on it expresses.
  }, [connection, includeUnmarked, nonce]);

  return { concepts, loading, error, readAt, registryAvailable, reread };
}

function conceptsFromReply(reply: Row | null): ComposableConcept[] {
  if (reply === null) return [];
  const raw = (reply as Record<string, unknown>)["concepts"];
  if (!Array.isArray(raw)) return [];
  return raw
    .filter((c): c is Record<string, unknown> => typeof c === "object" && c !== null)
    .map((c) => ({
      id: typeof c["id"] === "string" ? c["id"] : "",
      as: typeof c["as"] === "string" ? c["as"] : "",
      fields: Array.isArray(c["fields"]) ? c["fields"].filter((f): f is string => typeof f === "string") : [],
      list: typeof c["list"] === "string" ? c["list"] : "",
      description: typeof c["description"] === "string" ? c["description"] : "",
      marked: c["marked"] === true,
    }))
    .filter((c) => c.id !== "");
}

// ---------------------------------------------------------------------------
// Resolving a source list, for the Sources column's live count
// ---------------------------------------------------------------------------

export interface ResolvedSource {
  kind: string;
  ref: string;
  label: string;
  count: number;
  /**
   * Why this source found nothing, in words a person can act on.
   *
   * EMPTY WITH A ZERO COUNT IS A DIFFERENT ANSWER from a problem: "the
   * filter matched nothing" and "you cannot read that" are different
   * situations and the column draws them differently.
   */
  problem: string;
}

export interface ResolveState {
  sources: ResolvedSource[];
  total: number;
  loading: boolean;
  error: string;
}

/**
 * Reports what a source list finds, having composed nothing.
 *
 * IT EXISTS SO SOMEBODY SEES AN EMPTY SELECTION BEFORE THEY SPEND A MODEL
 * CALL DISCOVERING IT. Every read behind it runs under the caller's own
 * actor on the server, so the count is of rows they can see.
 */
export function useResolvedSources(sources: { kind: string; ref: string; label: string }[]): ResolveState {
  const connection = useOsConnection();
  const [state, setState] = useState<ResolveState>({
    sources: [],
    total: 0,
    loading: false,
    error: "",
  });

  // The KEY is the rendered source list. Without it this effect re-runs on
  // every render of the composer, which is every keystroke in the draft --
  // the registration-effect trap this shell has hit before.
  const key = JSON.stringify(sources);

  useEffect(() => {
    const query = connection?.query ?? null;
    if (query === null || sources.length === 0) {
      setState({ sources: [], total: 0, loading: false, error: "" });
      return;
    }
    let cancelled = false;
    setState((s) => ({ ...s, loading: true, error: "" }));
    void (async () => {
      try {
        const result = await query.composeResolveSources({ sources });
        if (cancelled) return;
        const reply = result.rows()[0] ?? null;
        setState({
          sources: resolvedFromReply(reply),
          total: typeof reply?.["total"] === "number" ? (reply["total"] as number) : 0,
          loading: false,
          error: "",
        });
      } catch (err) {
        if (cancelled) return;
        setState({
          sources: [],
          total: 0,
          loading: false,
          error: err instanceof Error ? err.message : String(err),
        });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [connection, key]);

  return state;
}

function resolvedFromReply(reply: Row | null): ResolvedSource[] {
  if (reply === null) return [];
  const raw = (reply as Record<string, unknown>)["sources"];
  if (!Array.isArray(raw)) return [];
  return raw
    .filter((s): s is Record<string, unknown> => typeof s === "object" && s !== null)
    .map((s) => ({
      kind: typeof s["kind"] === "string" ? s["kind"] : "",
      ref: typeof s["ref"] === "string" ? s["ref"] : "",
      label: typeof s["label"] === "string" ? s["label"] : "",
      count: typeof s["count"] === "number" ? s["count"] : 0,
      problem: typeof s["problem"] === "string" ? s["problem"] : "",
    }));
}
