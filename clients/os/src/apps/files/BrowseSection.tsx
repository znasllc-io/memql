import { useRef, useState } from "react";
import { ArrowUpDown, File, FileText, Folder, FolderPlus, HardDrive, Sparkles, Upload } from "lucide-react";
import { newShortId, type LiveState, type Row } from "@znasllc-io/memql-sdk-core/client";

import { ContextMenu } from "../../chrome/ContextMenu";
import { useOs } from "../../chrome/state";
import { useOsConnection } from "../../live/connection";
import { entriesOf, hasDirectory, walkEntries } from "../../items/folderDrop";
import { planArchive, runArchiveWalk } from "./actions/archive";
import {
  Button,
  Caption,
  Check,
  Chip,
  formatBytes,
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
import type { UploadTask, UploadTasksApi } from "./useUploadTasks";

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
  tasks,
  uploadFiles,
  uploadTree,
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
  tasks: UploadTask[];
  uploadFiles: UploadTasksApi["uploadFiles"];
  uploadTree: UploadTasksApi["uploadTree"];
}) {
  const { presence } = useMachines();
  const { actions } = useOs();
  const connection = useOsConnection();
  const patch = (p: Partial<FilesFilter>) => setFilter({ ...filter, ...p });
  const pickRef = useRef<HTMLInputElement | null>(null);
  // A refused DROP (over the file or depth bound) renders here, in surface,
  // with the walker's own sentence.
  const [dropRefusal, setDropRefusal] = useState("");

  // The rail's folder actions: a context menu per node, an inline rename,
  // and the archive flow -- confirm naming the LIVE count, then the
  // children-first walk with in-surface progress (design B5/D11).
  const [folderMenu, setFolderMenu] = useState<{ x: number; y: number; node: TreeNode } | null>(null);
  const [renamingFolderId, setRenamingFolderId] = useState("");
  const [pendingArchive, setPendingArchive] = useState<string>("");
  const [archiveProgress, setArchiveProgress] = useState<{ done: number; total: number } | null>(null);
  const [archiveError, setArchiveError] = useState("");
  const [railNote, setRailNote] = useState("");

  const runFolderArchive = async (folderId: string) => {
    const query = connection?.query ?? null;
    if (query === null) {
      setArchiveError("Not connected to the cluster, so nothing was archived.");
      return;
    }
    setPendingArchive("");
    setArchiveError("");
    // The plan recomputes from LIVE rows at the moment of running -- which
    // is what makes an interrupted walk idempotent: whatever already landed
    // is simply absent from the next plan.
    const plan = planArchive(tree, content, folderId);
    setArchiveProgress({ done: 0, total: plan.artifactIds.length + plan.folderIds.length });
    try {
      await runArchiveWalk(plan, {
        archiveArtifact: async (artifactId) => {
          await query.archiveArtifact({ artifactId });
        },
        archiveFolder: async (id) => {
          await query.archiveLibraryFolder({ folderId: id });
        },
        onProgress: (done, total) => setArchiveProgress({ done, total }),
      });
      if (filter.folderId === folderId) patch({ folderId: "" });
    } catch (err: unknown) {
      setArchiveError(err instanceof Error ? err.message : String(err));
    } finally {
      setArchiveProgress(null);
    }
  };

  const renameFolder = async (folderId: string, name: string) => {
    const query = connection?.query ?? null;
    setRenamingFolderId("");
    if (query === null || name.trim() === "") return;
    try {
      await query.renameLibraryFolder({ folderId, name: name.trim() });
    } catch (err: unknown) {
      setArchiveError(err instanceof Error ? err.message : String(err));
    }
  };

  const newFolderIn = async (parentFolderId: string) => {
    const query = connection?.query ?? null;
    if (query === null) {
      setArchiveError("Not connected to the cluster, so no folder was created.");
      return;
    }
    try {
      await query.createLibraryFolder({
        folderId: newShortId(),
        name: "New folder",
        ...(parentFolderId !== "" ? { parentFolderId } : {}),
      });
    } catch (err: unknown) {
      setArchiveError(err instanceof Error ? err.message : String(err));
    }
  };

  // Uploads land in the folder being LOOKED AT; searching means no folder is
  // being looked at, so the root takes them.
  const destinationFolderId = filter.search.trim() !== "" ? "" : (filter.folderId ?? "");

  const onDrop = (event: React.DragEvent) => {
    if (!event.dataTransfer.files.length && !event.dataTransfer.items.length) return;
    // BOTH PHASES STOP PROPAGATION (the Training rule): the app window sits
    // inside the desk plate and the desk plate takes file drops -- without
    // the stop, one file uploads twice to two different places.
    event.preventDefault();
    event.stopPropagation();
    const entries = entriesOf(event.dataTransfer);
    if (hasDirectory(entries)) {
      const label =
        entries.find((e) => e.isDirectory)?.name ?? `${entries.length} items`;
      void walkEntries(entries).then((walked) => {
        if (walked.refusal !== "") {
          setDropRefusal(walked.refusal);
          return;
        }
        uploadTree(walked.files, destinationFolderId, label);
      });
      return;
    }
    uploadFiles(Array.from(event.dataTransfer.files), destinationFolderId);
  };

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
    <div
      className="os-files"
      onDragOver={(event) => {
        if (!event.dataTransfer.types.includes("Files")) return;
        event.preventDefault();
        event.stopPropagation();
      }}
      onDrop={onDrop}
    >
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
        <Button tone="primary" onClick={() => pickRef.current?.click()}>
          <Upload size={13} aria-hidden /> Upload
        </Button>
        <input
          ref={pickRef}
          type="file"
          multiple
          hidden
          aria-label="Pick files to upload"
          onChange={(event) => {
            const files = Array.from(event.target.files ?? []);
            event.target.value = "";
            if (files.length > 0) uploadFiles(files, destinationFolderId);
          }}
        />
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
              renamingFolderId={renamingFolderId}
              onScope={(folderId) => patch({ folderId, search: "" })}
              onMenu={(x, y, menuNode) => setFolderMenu({ x, y, node: menuNode })}
              onRename={(folderId, name) => void renameFolder(folderId, name)}
              onCancelRename={() => setRenamingFolderId("")}
            />
          ))}
          <button
            type="button"
            className="os-files-node"
            disabled={connection === null}
            onClick={() => void newFolderIn(searching ? "" : (filter.folderId ?? ""))}
          >
            <FolderPlus size={14} aria-hidden />
            <span className="os-files-node-name">New folder</span>
          </button>
          {railNote !== "" ? <Caption>{railNote}</Caption> : null}
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
          {dropRefusal !== "" ? (
            <Notice tone="warn" sentence={dropRefusal}>
              <Button onClick={() => setDropRefusal("")}>Dismiss</Button>
            </Notice>
          ) : null}
          {pendingArchive !== "" ? (
            <Notice
              tone="warn"
              sentence={`Archive "${folderNameOf(pendingArchive)}" and its ${
                planArchive(tree, content, pendingArchive).itemCount
              } items?`}
              next="Everything inside archives too, children first. Nothing is deleted -- the archived filter brings it all back."
            >
              <div className="os-files-confirm">
                <Button tone="danger" onClick={() => void runFolderArchive(pendingArchive)}>
                  Archive
                </Button>
                <Button onClick={() => setPendingArchive("")}>Cancel</Button>
              </div>
            </Notice>
          ) : null}
          {archiveProgress !== null ? (
            <Caption>
              Archiving -- {archiveProgress.done} of {archiveProgress.total}. Interrupting is safe:
              running it again archives only the remainder.
            </Caption>
          ) : null}
          {archiveError !== "" ? (
            <Notice tone="error" sentence="The archive stopped." detail={archiveError}>
              <Button onClick={() => setArchiveError("")}>Dismiss</Button>
            </Notice>
          ) : null}
          {tasks.map((task) => (
            <UploadPlaceholder key={task.id} task={task} />
          ))}
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

      {folderMenu !== null ? (
        <ContextMenu
          x={folderMenu.x}
          y={folderMenu.y}
          label="Folder"
          entries={[
            {
              id: "open",
              label: "Open",
              onSelect: () => patch({ folderId: folderMenu.node.folder.id, search: "" }),
            },
            {
              id: "send",
              label: "Send to desk",
              onSelect: () => {
                const outcome = actions.sendFolderToDesk(
                  folderMenu.node.folder.id,
                  folderMenu.node.folder.name,
                );
                setRailNote(
                  outcome === "full"
                    ? "The desk is full -- remove something from it first."
                    : outcome === "focused"
                      ? "Already on the desk; it is selected there now."
                      : "On the desk.",
                );
                setTimeout(() => setRailNote(""), 5000);
              },
            },
            {
              id: "rename",
              label: "Rename",
              disabled: connection === null,
              onSelect: () => setRenamingFolderId(folderMenu.node.folder.id),
            },
            {
              id: "archive",
              label: "Archive",
              disabled: connection === null,
              onSelect: () =>
                confirmBeforeArchive
                  ? setPendingArchive(folderMenu.node.folder.id)
                  : void runFolderArchive(folderMenu.node.folder.id),
            },
          ]}
          onClose={() => setFolderMenu(null)}
        />
      ) : null}
    </div>
  );
}

function UploadPlaceholder({ task }: { task: UploadTask }) {
  const pct = task.totalBytes > 0 ? Math.min(100, (task.sentBytes / task.totalBytes) * 100) : 0;
  const counting =
    task.kind === "tree" ? `${task.doneFiles} of ${task.totalFiles} files · ` : "";
  return (
    <div className="os-files-up" data-state={task.state} aria-live="polite">
      <div className="os-files-up-line">
        <Upload size={14} aria-hidden />
        <span className="os-files-up-name">{task.label}</span>
        <span className="os-files-up-bytes">
          {task.state === "done"
            ? "landed"
            : `${counting}${formatBytes(task.sentBytes)} of ${formatBytes(task.totalBytes)}`}
        </span>
        {task.state === "sending" ? <Button onClick={task.abort}>Cancel</Button> : null}
        {task.state === "failed" && task.retry ? (
          <Button onClick={task.retry}>Try again</Button>
        ) : null}
        {task.state === "failed" ? <Button onClick={task.dismiss}>Dismiss</Button> : null}
      </div>
      {task.state === "sending" ? (
        <div className="os-files-up-track" aria-hidden>
          <div className="os-files-up-bar" style={{ width: `${pct}%` }} />
        </div>
      ) : null}
      {task.resumedChunks !== undefined && task.totalChunks !== undefined && task.state === "sending" ? (
        <p className="os-files-up-note">
          Resuming -- {task.resumedChunks} of {task.totalChunks} chunks already in the cluster.
        </p>
      ) : null}
      {task.state === "failed" && task.kind === "file" ? (
        // THE SERVER'S SENTENCE, VERBATIM. The client duplicates no limit:
        // over-cap and over-quota arrive as the engine's own words, which
        // name both numbers.
        <p className="os-files-up-note" role="alert">
          {task.error}
        </p>
      ) : null}
      {task.state === "failed" && task.kind === "tree" ? (
        <>
          <p className="os-files-up-note" role="alert">
            {task.error} The landed files stay landed.
          </p>
          {task.failures.map((failure) => (
            <div key={failure.name} className="os-files-up-actions">
              <span className="os-files-up-note">
                {failure.name} -- {failure.error}
              </span>
              <Button onClick={failure.retry}>Try again</Button>
            </div>
          ))}
        </>
      ) : null}
    </div>
  );
}

function RailNode({
  node,
  counts,
  currentId,
  renamingFolderId,
  onScope,
  onMenu,
  onRename,
  onCancelRename,
}: {
  node: TreeNode;
  counts: Map<string, number>;
  currentId: string | null;
  renamingFolderId: string;
  onScope: (folderId: string) => void;
  onMenu: (x: number, y: number, node: TreeNode) => void;
  onRename: (folderId: string, name: string) => void;
  onCancelRename: () => void;
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
  const renaming = renamingFolderId === node.folder.id;
  return (
    <>
      {renaming ? (
        <div className="os-files-node" style={{ paddingInlineStart: `${10 + node.depth * 14}px` }}>
          <Folder size={14} aria-hidden />
          <input
            className="os-input os-files-rename"
            defaultValue={node.folder.name}
            aria-label={`Rename ${node.folder.name}`}
            autoFocus
            onKeyDown={(event) => {
              if (event.key === "Enter") onRename(node.folder.id, event.currentTarget.value);
              if (event.key === "Escape") onCancelRename();
            }}
            onBlur={(event) => onRename(node.folder.id, event.currentTarget.value)}
          />
        </div>
      ) : (
        <button
          type="button"
          className="os-files-node"
          style={{ paddingInlineStart: `${10 + node.depth * 14}px` }}
          data-current={currentId === node.folder.id ? true : undefined}
          onClick={() => onScope(node.folder.id)}
          onContextMenu={(event) => {
            // The rail offers its own menu, so the browser's stays out (the
            // shell's right-click rule: a surface with a menu says so).
            event.preventDefault();
            event.stopPropagation();
            onMenu(event.clientX, event.clientY, node);
          }}
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
      )}
      {node.children.map((child) => (
        <RailNode
          key={child.folder.id}
          node={child}
          counts={counts}
          currentId={currentId}
          renamingFolderId={renamingFolderId}
          onScope={onScope}
          onMenu={onMenu}
          onRename={onRename}
          onCancelRename={onCancelRename}
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
