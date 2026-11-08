import { useEffect } from "react";
import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";
import { useLiveCollection, usePagedCollection, type PagedCollectionHandle } from "../../live/useLiveCollection";
import { useDebouncedValue } from "../../logs/useDebouncedValue";
import { useGraphRevision } from "../../live/useGraphRevision";
import { ARTIFACT_CONCEPT, COMPOSITION_CONCEPT, FILE_CONCEPT, FOLDER_CONCEPT } from "./concepts";
import { applyFilters, type DeskMembership, type FilesFilter } from "./filters";
import { artifactFromRow, compositionFromRow } from "./rows";
import { materializedByArtifactId } from "./fold";

const FACET_INPUTS = [ARTIFACT_CONCEPT] as const;

export function libraryBrowseArgs(filter: FilesFilter, desk: DeskMembership, sourceRefs?: string[]) {
  const searching = filter.search.trim() !== "";
  return {
    archived: filter.place === "bin",
    ...(!searching && filter.folderId ? { folderId: filter.folderId } : {}),
    ...(filter.kind !== "all" ? { kind: filter.kind } : {}),
    ...(filter.source !== "all" ? { source: filter.source } : {}),
    ...(filter.accountId !== "all" ? { accountId: filter.accountId === "none" ? "" : filter.accountId } : {}),
    ...(filter.label ? { label: filter.label } : {}),
    ...(searching ? { search: filter.search.trim() } : {}),
    ...(filter.place === "desktop" && (searching || !filter.folderId) ? {
      ids: [...desk.fileArtifactIds].sort(), folderIds: searching ? [...desk.folderIds].sort() : [],
    } : {}),
    ...(filter.place === "materializer" ? { sourceRefs: sourceRefs ?? [] } : {}),
  };
}

/** Directory pages are retained for the authenticated connection. Related rows
 * are read in batches for the visible files; no per-file request waterfall. */
export function useLibraryFeeds(filter: FilesFilter, desk: DeskMembership, enabled = true, backups = false) {
  const search = useDebouncedValue(filter.search, 250);
  const effectiveFilter = { ...filter, search };
  const outputs = useLiveCollection<Row>(enabled && filter.place === "materializer" ? "files:output-index" : null, connection => ({
    concept: COMPOSITION_CONCEPT,
    seed: async (cursor, signal) => {
      const result = await connection.query.compositionOutputIndex({}, { signal, ...(cursor ? { cursor } : {}) });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
  }));
  const outputIds = outputs.snapshot.rows.map(compositionFromRow).map(row => row.outputFileId).filter(Boolean);
  const sourceRefs = outputIds.flatMap(id => [id, `v1:library:file:${id}`]).sort();
  const args = libraryBrowseArgs(effectiveFilter, desk, sourceRefs);
  const argsKey = JSON.stringify(args);
  const ready = enabled && (filter.place !== "materializer" || outputs.snapshot.state !== "seeding");
  const artifacts = usePagedCollection<Row>(ready ? `files:browse:${filter.sortAscending}:${argsKey}` : null, connection => ({
    concept: ARTIFACT_CONCEPT,
    comparePageRows: (a, b) => {
      const left = artifactFromRow(a), right = artifactFromRow(b);
      const order = left.createdAt.localeCompare(right.createdAt) || left.id.localeCompare(right.id);
      return filter.sortAscending ? order : -order;
    },
    seed: async (cursor, signal) => {
      const result = await connection.query[filter.sortAscending ? "libraryArtifactsBrowseOldest" : "libraryArtifactsBrowse"](
        args, { signal, ...(cursor ? { cursor } : {}) },
      );
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    inScope: row => {
      const artifact = artifactFromRow(row);
      const materialized = materializedByArtifactId(outputs.snapshot.rows.map(compositionFromRow), [artifact]);
      return applyFilters([artifact], effectiveFilter, desk, materialized).length > 0;
    },
    reread: async (id, signal) => (await getRowByConceptAndId(connection.query, ARTIFACT_CONCEPT, id, { signal }) as Row) ?? null,
  }));
  const folders = useLiveCollection<Row>(enabled || backups ? "files:folders" : null, connection => ({
    concept: FOLDER_CONCEPT,
    seed: async (cursor, signal) => {
      const result = await connection.query.libraryFolderDirectory({}, { signal, ...(cursor ? { cursor } : {}) });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (id, signal) => (await getRowByConceptAndId(connection.query, FOLDER_CONCEPT, id, { signal }) as Row) ?? null,
  }));
  const fileIds = [...new Set(artifacts.snapshot.rows.map(artifactFromRow).filter(row => row.kind === "file")
    .map(row => row.sourceConceptRef.split(":").pop() ?? "").filter(Boolean))].sort();
  const fileKey = JSON.stringify(fileIds);
  // Backing bytes/origin metadata belongs to the selected Inspector. Only
  // Backups needs the complete file-state population for its folder health.
  const files = useLiveCollection<Row>(backups ? "files:backup-files" : null, connection => ({
    concept: FILE_CONCEPT,
    seed: async (cursor, signal) => {
      const result = await connection.query.libraryFilesForOwner({}, { signal, ...(cursor ? { cursor } : {}) });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (id, signal) => (await getRowByConceptAndId(connection.query, FILE_CONCEPT, id, { signal }) as Row) ?? null,
  }));
  const visibleOutputs = useLiveCollection<Row>(enabled && filter.place !== "materializer" && fileIds.length ? `files:visible-outputs:${fileKey}` : null, connection => ({
    concept: COMPOSITION_CONCEPT,
    inScope: row => fileIds.includes(compositionFromRow(row).outputFileId),
    seed: async (cursor, signal) => {
      const result = await connection.query.compositionOutputIndex({ fileIds }, { signal, ...(cursor ? { cursor } : {}) });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
  }));
  const facets = useLiveCollection<Row>(ready ? `files:facets:${argsKey}` : null, connection => ({
    concept: ARTIFACT_CONCEPT, actions: [], paged: false,
    seed: async (_cursor, signal) => {
      const result = await connection.query.libraryBrowseFacets(args, { signal });
      const row = result.single();
      if (!row || !Array.isArray(row.folders) || !Array.isArray(row.labelGroups)) throw new Error("The Library counts could not be read.");
      return { rows: [{ ...row, id: "facets" }], nextCursor: "" };
    },
  }));
  const revision = useGraphRevision(FACET_INPUTS, ready, { externalRecovery: true });
  useEffect(() => { if (revision) facets.reseed(); }, [revision, facets.reseed]);
  const index = facets.snapshot.rows[0];
  const counts = index ? new Map((index.folders as {folderId: string; count: number}[]).map(row => [row.folderId, row.count])) : null;
  const labels = index ? [...new Set((index.labelGroups as string[][]).flat())].filter(Boolean).sort((a,b) => a.localeCompare(b)) : [];
  return { artifacts, folders, files, compositions: filter.place === "materializer" ? outputs : visibleOutputs,
    counts, labels, facetsError: facets.snapshot.error, effectiveFilter };
}

export type LibraryArtifactsFeed = PagedCollectionHandle<Row>;
