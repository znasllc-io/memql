import { ArrowUpDown, File, FileText, Folder, HardDrive, Sparkles } from "lucide-react";
import type { LiveState, Row } from "@znasllc-io/memql-sdk-core/client";

import {
  Button,
  Caption,
  Check,
  Chip,
  Input,
  LiveList,
  Notice,
  ProvenanceDot,
  Row as ListRow,
  Select,
} from "../../kit";
import { useMachines } from "../../live/machines";
import type { LiveView } from "../../live/liveView";
import type { LiveCollectionHandle } from "../../live/useLiveCollection";
import { SOURCE_VALUES } from "./concepts";
import type { FilesFilter, KindFilter } from "./filters";
import type { FolderTree, TreeNode } from "./fold";
import { artifactFingerprint, artifactName, fileStory, type ArtifactRow } from "./rows";
import { Inspector } from "./Inspector";

// The browse (design D1): rail, list, inspector -- three readings of the two
// feeds the app root retains, sharing one selection.

const KIND_TABS: Array<{ value: KindFilter; label: string }> = [
  { value: "all", label: "All" },
  { value: "file", label: "Files" },
  { value: "document", label: "Documents" },
  { value: "generated_output", label: "Generated" },
];

export function kindGlyph(kind: string, size = 16) {
  if (kind === "document") return <FileText size={size} aria-hidden />;
  if (kind === "generated_output") return <Sparkles size={size} aria-hidden />;
  return <File size={size} aria-hidden />;
}

export function BrowseSection({
  list,
  artifacts,
  foldersState,
  tree,
  content,
  filter,
  setFilter,
  selectedId,
  onSelect,
  confirmBeforeArchive,
  askContext,
}: {
  list: LiveView<ArtifactRow> | null;
  artifacts: LiveCollectionHandle<Row>;
  foldersState: LiveState;
  tree: FolderTree;
  content: ArtifactRow[];
  filter: FilesFilter;
  setFilter: (next: FilesFilter) => void;
  selectedId: string;
  onSelect: (id: string) => void;
  confirmBeforeArchive: boolean;
  askContext: (tag: string) => void;
}) {
  const { presence } = useMachines();
  const patch = (p: Partial<FilesFilter>) => setFilter({ ...filter, ...p });

  const selected = content.find((r) => r.id === selectedId) ?? null;
  const searching = filter.search.trim() !== "";

  // Direct, non-archived counts by folder -- what a person would count.
  const counts = new Map<string, number>();
  for (const row of content) {
    if (row.archived) continue;
    counts.set(row.folderId, (counts.get(row.folderId) ?? 0) + 1);
  }

  const folderNameOf = (folderId: string): string => {
    if (folderId === "") return "Library";
    return tree.byId.get(folderId)?.folder.name ?? folderId;
  };

  // Empty and filtered-to-empty are DIFFERENT answers: one is about the
  // Library, the other about the question just asked of it.
  const narrowed =
    searching || filter.kind !== "all" || filter.source !== "all" || filter.folderId !== "";
  const emptyText =
    content.filter((r) => !r.archived).length === 0 && !narrowed
      ? "Nothing in your Library yet. Drop a file onto the desk or upload one here."
      : "Nothing matches. Clear the search or filters to see your files.";

  const scopeLabel = searching
    ? "Search results across your Library"
    : filter.folderId === ""
      ? "Library"
      : folderNameOf(filter.folderId ?? "");

  return (
    <div className="os-files">
      <div className="os-files-toolbar" role="toolbar" aria-label="Filter files">
        <div className="os-files-search">
          <Input
            id="files-search"
            label="Search files"
            placeholder="Search your Library"
            value={filter.search}
            onChange={(search) => patch({ search })}
          />
        </div>
        <div className="os-files-kinds" role="radiogroup" aria-label="Kind">
          {KIND_TABS.map((tab) => (
            <button
              key={tab.value}
              type="button"
              role="radio"
              aria-checked={filter.kind === tab.value}
              className="os-files-kind"
              title={tab.value === "all" ? "every kind" : tab.value}
              onClick={() => patch({ kind: tab.value })}
            >
              {tab.label}
            </button>
          ))}
        </div>
        <Select
          id="files-source"
          label="Source"
          value={filter.source}
          onChange={(source) => patch({ source: source as FilesFilter["source"] })}
        >
          <option value="all">Any source</option>
          {SOURCE_VALUES.map((source) => (
            <option key={source} value={source}>
              {source}
            </option>
          ))}
        </Select>
        <Check checked={filter.showArchived} onChange={(showArchived) => patch({ showArchived })}>
          Archived
        </Check>
        <Button
          onClick={() => patch({ sortAscending: !filter.sortAscending })}
          ariaLabel={filter.sortAscending ? "Sorted oldest first" : "Sorted newest first"}
        >
          <ArrowUpDown size={13} aria-hidden /> {filter.sortAscending ? "Oldest" : "Newest"}
        </Button>
      </div>

      <div className="os-files-body">
        <nav className="os-files-rail" aria-label="Folders">
          <button
            type="button"
            className="os-files-node"
            data-current={!searching && filter.folderId === "" ? true : undefined}
            onClick={() => patch({ folderId: "", search: "" })}
          >
            <HardDrive size={14} aria-hidden />
            <span className="os-files-node-name">Library</span>
            <span className="os-files-node-count">{counts.get("") ?? 0}</span>
          </button>
          {tree.roots.map((node) => (
            <RailNode
              key={node.folder.id}
              node={node}
              counts={counts}
              currentId={searching ? null : filter.folderId}
              onScope={(folderId) => patch({ folderId, search: "" })}
            />
          ))}
          {foldersState === "degraded" || foldersState === "disconnected" ? (
            <Caption>Folder updates are behind -- showing the last known tree.</Caption>
          ) : null}
        </nav>

        <div className="os-files-list">
          <p className="os-files-scope os-caption" aria-live="off">
            {scopeLabel}
          </p>
          {artifacts.snapshot.error ? (
            <Notice
              tone="error"
              sentence="This cluster did not return your Library."
              next="The engine decides which rows reach you; your own always do."
            >
              <Button onClick={artifacts.reseed}>Try again</Button>
            </Notice>
          ) : null}
          <LiveList<ArtifactRow>
            key={`${filter.folderId ?? "~"}|${filter.kind}|${filter.source}|${filter.showArchived}|${filter.search}`}
            source={list}
            rowId={(r) => r.id}
            fingerprint={artifactFingerprint}
            label={scopeLabel}
            emptyText={emptyText}
            renderRow={(row, tick) => (
              <FileLine
                row={row}
                tick={tick}
                searching={searching}
                folderNameOf={folderNameOf}
                presence={presence}
                open={selectedId === row.id}
                onToggle={() => onSelect(selectedId === row.id ? "" : row.id)}
              />
            )}
          />
        </div>

        {selected === null ? null : (
          <Inspector
            key={selected.id}
            row={selected}
            folderNameOf={folderNameOf}
            tree={tree}
            presence={presence}
            confirmBeforeArchive={confirmBeforeArchive}
            onAsk={askContext}
            onClose={() => onSelect("")}
          />
        )}
      </div>
    </div>
  );
}

function RailNode({
  node,
  counts,
  currentId,
  onScope,
}: {
  node: TreeNode;
  counts: Map<string, number>;
  currentId: string | null;
  onScope: (folderId: string) => void;
}) {
  const marker =
    node.placement === "orphan"
      ? { label: "parent gone", title: "This folder's parent is archived or missing, so it shows at the top." }
      : node.placement === "cycle"
        ? { label: "loop", title: "This folder's ancestry loops back on itself, so it shows at the top." }
        : node.placement === "deep"
          ? { label: "too deep", title: "Nested past 12 levels, so it shows at the top." }
          : null;
  const count = counts.get(node.folder.id) ?? 0;
  return (
    <>
      <button
        type="button"
        className="os-files-node"
        style={{ paddingInlineStart: `${10 + node.depth * 14}px` }}
        data-current={currentId === node.folder.id ? true : undefined}
        onClick={() => onScope(node.folder.id)}
      >
        <Folder size={14} aria-hidden />
        <span className="os-files-node-name">{node.folder.name}</span>
        {marker ? (
          <Chip tone="muted" title={marker.title}>
            {marker.label}
          </Chip>
        ) : null}
        <span className="os-files-node-count">{count > 0 ? count : ""}</span>
      </button>
      {node.children.map((child) => (
        <RailNode
          key={child.folder.id}
          node={child}
          counts={counts}
          currentId={currentId}
          onScope={onScope}
        />
      ))}
    </>
  );
}

function FileLine({
  row,
  tick,
  searching,
  folderNameOf,
  presence,
  open,
  onToggle,
}: {
  row: ArtifactRow;
  tick: "added" | "updated" | null;
  searching: boolean;
  folderNameOf: (folderId: string) => string;
  presence: (workerId: string) => { name?: string; online: boolean } | null;
  open: boolean;
  onToggle: () => void;
}) {
  const story = fileStory(row, row.producedByWorkerId ? presence(row.producedByWorkerId) : null);
  const extraLabels = row.labels.length > 2 ? row.labels.length - 2 : 0;
  return (
    <ListRow
      icon={kindGlyph(row.kind)}
      name={artifactName(row)}
      current={!row.archived}
      dim={row.archived}
      open={open}
      onOpen={onToggle}
      state={
        <>
          <ProvenanceDot tone={story.tone} label={story.sentence || undefined} />
          {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
        </>
      }
    >
      {searching && row.folderId !== "" ? (
        <Chip tone="muted">in {folderNameOf(row.folderId)}</Chip>
      ) : null}
      {row.labels.slice(0, 2).map((label) => (
        <Chip key={label} tone="neutral">
          {label}
        </Chip>
      ))}
      {extraLabels > 0 ? <Chip tone="muted">+{extraLabels}</Chip> : null}
      {row.kind === "document" && row.validationStatus !== "" ? (
        <Chip tone="muted" title="The training pipeline's verdict on this document">
          {row.validationStatus}
        </Chip>
      ) : null}
      {row.archived ? <Chip tone="muted">archived</Chip> : null}
    </ListRow>
  );
}
