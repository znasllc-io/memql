import { useCallback, useEffect, useMemo, useState } from "react";
import {
  Concepts,
  getRowByConceptAndId,
  refusalSentence,
  rowString,
  rowBool,
  sameEntityId,
  type Result,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import { useDebouncedValue } from "../../logs/useDebouncedValue";
import type { LiveListSource } from "../../live/LiveList";
import { useOsConnection } from "../../live/connection";
import {
  useLiveCollection,
  usePagedCollection,
  type PagedCollectionHandle,
  type LiveCollectionHandle,
} from "../../live/useLiveCollection";
import { compositionRunSource, unfinishedCompositionRunIds } from "./runState";
import {
  COMPOSITION_CONCEPT,
  RECIPE_CONCEPT,
  TEMPLATE_CONCEPT,
} from "./concepts";

// Directory pages and selected records have independent keys. Complete picker
// catalogs are requested only by the editor that needs their choices.

/** What one read is asked for to get one page: its abort signal, and the cursor once the page before named one. */
function pageOptions(
  cursor: string,
  signal: AbortSignal,
): { signal: AbortSignal; cursor?: string } {
  return cursor === "" ? { signal } : { signal, cursor };
}

/** What a seed hands the collection for one page of a read: its rows, and the cursor of the next page ("" after the last). */
function pageOf(result: Result): { rows: Row[]; nextCursor: string } {
  return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
}

export interface ComposeBrowseFilter { search?: string; format?: string; includeArchived?: boolean }

function useComposeDirectory(kind: "composition" | "template" | "recipe", enabled: boolean, filter: ComposeBrowseFilter): PagedCollectionHandle<Row> {
  const search = useDebouncedValue(filter.search ?? "", 250).trim();
  const args = { search, format: filter.format ?? "", includeArchived: filter.includeArchived ?? false };
  const concept = kind === "composition" ? COMPOSITION_CONCEPT : kind === "template" ? TEMPLATE_CONCEPT : RECIPE_CONCEPT;
  return usePagedCollection<Row>(enabled ? `compose:directory:${kind}:${JSON.stringify(args)}` : null, connection => ({
    concept,
    seed: async (cursor, signal) => pageOf(await connection.query[kind === "composition" ? "compositionsBrowse" : kind === "template" ? "composeTemplatesBrowse" : "composeRecipesBrowse"](args, pageOptions(cursor, signal))),
    inScope: row => (args.includeArchived || !rowBool(row, "archived"))
      && (!args.format || rowString(row, "format") === args.format)
      && (!search || [rowString(row, "name"), rowString(row, kind === "composition" ? "statement" : "description")].some(value => value.toLowerCase().includes(search.toLowerCase()))),
    reread: async (id, signal) => (await getRowByConceptAndId(connection.query, concept, id, { signal }) as Row) ?? null,
  }));
}

export const useTemplateDirectory = (filter: ComposeBrowseFilter) => useComposeDirectory("template", true, filter);
export const useRecipeDirectory = (filter: ComposeBrowseFilter) => useComposeDirectory("recipe", true, filter);

export function useComposeRecord(kind: "composition" | "template" | "recipe", id: string): LiveCollectionHandle<Row> {
  const concept = kind === "composition" ? COMPOSITION_CONCEPT : kind === "template" ? TEMPLATE_CONCEPT : RECIPE_CONCEPT;
  return useLiveCollection<Row>(id ? `compose:record:${kind}:${id}` : null, connection => ({
    concept, paged: false, eventInScope: candidate => sameEntityId(candidate, id),
    inScope: row => sameEntityId(rowString(row, "id"), id),
    seed: async (_cursor, signal) => {
      const result = kind === "composition" ? await connection.query.compositionById({ compositionId: id }, { signal })
        : kind === "template" ? await connection.query.composeTemplateById({ templateId: id }, { signal })
        : await connection.query.composeRecipeById({ recipeId: id }, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => (await getRowByConceptAndId(connection.query, concept, rowId, { signal }) as Row) ?? null,
  }));
}

/** The selected record is independent of recent pages. No old link forces a
 * catalog walk, and recovery keeps all pages the person explicitly loaded. */
export function useCompositions(enabled = true, filter: ComposeBrowseFilter = {}, selectedId = ""): PagedCollectionHandle<Row> & { runOf(runId: string): Row | undefined } {
  const directory = useComposeDirectory("composition", enabled, filter);
  const detail = useComposeRecord("composition", selectedId);
  const combined = useMemo(() => {
    if (!directory.source || !detail.source) return directory.source ?? detail.source;
    return withSelectedRecord(directory.source, detail.source, selectedId);
  }, [directory.source, detail.source, selectedId]);
  const compositions = { ...directory, source: combined, snapshot: combined?.snapshot ?? directory.snapshot };
  // Read the exact bound runs, including old ones outside the newest run page.
  // They remain retained while these compositions are unfinished; neither the
  // worker process nor Nexus needs to remain open for an abandonment to arrive.
  const runIds = unfinishedCompositionRunIds(compositions.snapshot.rows);
  const runKey = JSON.stringify(runIds);
  const runs = useLiveCollection<Row>(
    runIds.length ? `compose:runs:${runKey}` : null,
    (connection) => ({
      concept: Concepts.WORK_RUN,
      eventInScope: id => runIds.some(runId => sameEntityId(runId, id)),
      seed: async (cursor, signal) => {
        const result = await connection.query.workRunSummariesByIds({ runIds }, pageOptions(cursor, signal));
        return pageOf(result);
      },
      inScope: (row) => runIds.some(id => sameEntityId(id, rowString(row, "id"))),
      reread: async (runId, signal) => {
        if (!runIds.some(id => sameEntityId(id, runId))) return null;
        const result = await connection.query.workRunForOwner(
          { runId },
          { signal },
        );
        return result.rows()[0] ?? null;
      },
      paged: true,
    }),
  );
  const source = useMemo(
    () =>
      compositions.source && runs.source
        ? compositionRunSource(compositions.source, runs.source)
        : compositions.source,
    [compositions.source, runs.source],
  );
  // Ids are compared as the SDK compares them: one side may be canonical and the other bare.
  const runRows = runs.snapshot.rows;
  const runOf = useCallback(
    (runId: string) =>
      runRows.find((run) => sameEntityId(rowString(run, "id"), runId)),
    [runRows],
  );
  return {
    ...directory,
    source,
    snapshot: source?.snapshot ?? compositions.snapshot,
    reseed: () => {
      compositions.reseed();
      detail.reseed();
      runs.reseed();
    },
    runOf,
  };
}

/** Every template binding this caller owns, every page. */
export function useTemplates(enabled = true): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>(enabled ? "compose:templates" : null, (connection) => ({
    concept: TEMPLATE_CONCEPT,
    seed: async (cursor, signal) =>
      pageOf(
        await connection.query.composeTemplates(
          {},
          pageOptions(cursor, signal),
        ),
      ),
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(
        connection.query,
        TEMPLATE_CONCEPT,
        rowId,
        { signal },
      );
      return (row as Row) ?? null;
    },
  }));
}

/** Every recipe this caller owns, every page. */
export function useRecipes(enabled = true): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>(enabled ? "compose:recipes" : null, (connection) => ({
    concept: RECIPE_CONCEPT,
    seed: async (cursor, signal) =>
      pageOf(
        await connection.query.composeRecipes({}, pageOptions(cursor, signal)),
      ),
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(
        connection.query,
        RECIPE_CONCEPT,
        rowId,
        { signal },
      );
      return (row as Row) ?? null;
    },
  }));
}

/**
 * The person's Library file rows (`libraryFilesForOwner`, every page): name, size, archived and the
 * file's own id, which the From tray offers and the routing reads. No read while `enabled` is false.
 *
 * The file rows, not the artifact index, because the routing judges a picked file by its name and
 * size and the file's own id is the bare id a `library_file` source names (spec D32); the index
 * carries neither. It is Files' own `files:files` feed over again (apps/files/useLibrary.ts), kept
 * here under the Materializer's key so closing a wizard lets it go.
 */
export function useLibraryFiles(enabled: boolean): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>(
    enabled ? "compose:library-files" : null,
    (connection) => ({
      concept: Concepts.LIBRARY_FILE,
      seed: async (cursor, signal) =>
        pageOf(
          await connection.query.libraryFilesForOwner(
            {},
            pageOptions(cursor, signal),
          ),
        ),
      reread: async (rowId, signal) => {
        const row = await getRowByConceptAndId(
          connection.query,
          Concepts.LIBRARY_FILE,
          rowId,
          { signal },
        );
        return (row as Row) ?? null;
      },
    }),
  );
}

/**
 * The person's live folders (`libraryFolders`, every page), for the Folder step and a made file's
 * "Filed in". No read while `enabled` is false. The read carries `archived != true` and not-deleted,
 * so an archived folder is not here; the Folder step shows one a setup claimed as "Unavailable folder".
 */
export function useFolders(enabled: boolean): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>(
    enabled ? "compose:folders" : null,
    (connection) => ({
      concept: Concepts.LIBRARY_FOLDER,
      seed: async (cursor, signal) =>
        pageOf(
          await connection.query.libraryFolders(
            {},
            pageOptions(cursor, signal),
          ),
        ),
      reread: async (rowId, signal) => {
        const row = await getRowByConceptAndId(
          connection.query,
          Concepts.LIBRARY_FOLDER,
          rowId,
          { signal },
        );
        return (row as Row) ?? null;
      },
    }),
  );
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

export function useComposableConcepts(
  includeUnmarked: boolean,
  enabled = true,
): ComposablesState {
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
    if (!enabled || query === null) return;
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
        // The cluster's sentence alone, without the call's name or the engine's frame.
        setError(refusalSentence(err));
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
    // `includeUnmarked` is a DIFFERENT READING and re-baselines, which is
    // what keying on it expresses.
  }, [connection, includeUnmarked, nonce, enabled]);

  return { concepts, loading, error, readAt, registryAvailable, reread };
}

function conceptsFromReply(reply: Row | null): ComposableConcept[] {
  if (reply === null) return [];
  const raw = (reply as Record<string, unknown>)["concepts"];
  if (!Array.isArray(raw)) return [];
  return raw
    .filter(
      (c): c is Record<string, unknown> => typeof c === "object" && c !== null,
    )
    .map((c) => ({
      id: typeof c["id"] === "string" ? c["id"] : "",
      as: typeof c["as"] === "string" ? c["as"] : "",
      fields: Array.isArray(c["fields"])
        ? c["fields"].filter((f): f is string => typeof f === "string")
        : [],
      list: typeof c["list"] === "string" ? c["list"] : "",
      description: typeof c["description"] === "string" ? c["description"] : "",
      marked: c["marked"] === true,
    }))
    .filter((c) => c.id !== "");
}

// ---------------------------------------------------------------------------
// Resolving a source list, for the ingredient tray
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
export function useResolvedSources(
  sources: { kind: string; ref: string; label: string }[],
): ResolveState {
  const connection = useOsConnection();
  const [state, setState] = useState<ResolveState>({
    sources: [],
    total: 0,
    loading: false,
    error: "",
  });

  // The KEY is the rendered source list. Without it this effect re-runs on
  // every render of the composer, which is every keystroke in the brief --
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
          total:
            typeof reply?.["total"] === "number"
              ? (reply["total"] as number)
              : 0,
          loading: false,
          error: "",
        });
      } catch (err) {
        if (cancelled) return;
        setState({
          sources: [],
          total: 0,
          loading: false,
          // The cluster's sentence alone, without the call's name or the engine's frame.
          error: refusalSentence(err),
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
    .filter(
      (s): s is Record<string, unknown> => typeof s === "object" && s !== null,
    )
    .map((s) => ({
      kind: typeof s["kind"] === "string" ? s["kind"] : "",
      ref: typeof s["ref"] === "string" ? s["ref"] : "",
      label: typeof s["label"] === "string" ? s["label"] : "",
      count: typeof s["count"] === "number" ? s["count"] : 0,
      problem: typeof s["problem"] === "string" ? s["problem"] : "",
    }));
}

/** Preserve page metadata and reconcile a selected row through its own read. */
function withSelectedRecord(directory: LiveListSource<Row>, detail: LiveListSource<Row>, selectedId: string): LiveListSource<Row> {
  let previousDirectory: typeof directory.snapshot | null = null;
  let previousDetail: typeof detail.snapshot | null = null;
  let snapshot: typeof directory.snapshot;
  return {
    subscribe: notify => { const a = directory.subscribe(notify), b = detail.subscribe(notify); return () => { a(); b(); }; },
    get snapshot() {
      const a = directory.snapshot, b = detail.snapshot;
      if (a === previousDirectory && b === previousDetail) return snapshot;
      previousDirectory = a; previousDetail = b;
      const rows = b.state === "seeding" ? a.rows : [...a.rows.filter(row => !sameEntityId(rowString(row, "id"), selectedId)), ...b.rows];
      snapshot = { ...a, rows, version: a.version + b.version, error: a.error || b.error,
        state: b.state === "disconnected" || b.state === "degraded" || b.state === "seeding" && !a.rows.some(row => sameEntityId(rowString(row, "id"), selectedId)) ? b.state : a.state };
      return snapshot;
    },
  };
}
