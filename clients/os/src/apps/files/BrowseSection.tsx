import { useRef, useState } from "react";
import {
  Archive as ArchiveGlyph,
  File,
  FileText,
  Folder,
  FolderPlus,
  HardDrive,
  Monitor,
  Sparkles,
  Upload,
} from "lucide-react";
import { newShortId, type LiveState, type Row } from "@znasllc-io/memql-sdk-core/client";

import { ContextMenu } from "../../chrome/ContextMenu";
import { useOs } from "../../chrome/state";
import { useOsConnection } from "../../live/connection";
import { entriesOf, hasDirectory, walkEntries } from "../../items/folderDrop";
import { planArchive, runArchiveWalk } from "./actions/archive";
import {
  Button,
  Caption,
  Chip,
  formatBytes,
  Head,
  LiveList,
  Notice,
  ProvenanceDot,
  Refine,
  Row as ListRow,
  Select,
  SortControl,
  type RefineChip,
} from "../../kit";
import { useMachines } from "../../live/machines";
import type { LiveView } from "../../live/liveView";
import type { LiveCollectionHandle } from "../../live/useLiveCollection";
import { useDraggable } from "@dnd-kit/core";
import { SOURCE_VALUES } from "./concepts";
import type { FilesFilter, FilesPlace, KindFilter } from "./filters";
import type { FolderRow } from "./rows";
import type { FolderTree, TreeNode } from "./fold";
import { LINK_LABEL, LINK_SENTENCE, type LinkState } from "./links";
import type { BinDropPayload } from "../bin/concepts";
import { binItemFromArtifact } from "../bin/rows";
import { planRestore, runRestore } from "../bin/restore";
import { artifactFingerprint, artifactName, fileStory, type ArtifactRow } from "./rows";
import { accountIsArchived, accountName } from "../accounts/rows";
import { useAccountOptions } from "../accounts/tie";
import { ACCOUNT_ANY, ACCOUNT_NONE } from "./filters";
import { Inspector } from "./Inspector";
import type { UploadProvider } from "../../items/upload";
import type { UploadTask, UploadTasksApi } from "./useUploadTasks";

// The browse (design D1, reshaped by epic memql#4842): rail, list, inspector
// -- three readings of the feeds the app root retains, sharing one selection.
// The rail offers three PLACES -- Library, Desktop, Archive -- and the top of
// the section is a Head, a quiet sort, one Refine affordance and the Upload
// primary (DESIGN.md rules 1-3): filter chrome appears when a question is
// being asked, never as furniture.

const KIND_TABS: Array<{ value: KindFilter; label: string }> = [
  { value: "all", label: "All" },
  { value: "file", label: "Files" },
  { value: "document", label: "Documents" },
  { value: "generated_output", label: "Generated" },
];

/** A desk folder shortcut, folded from shell state by the app root. */
export interface DeskFolderShortcut {
  folderId: string;
  name: string;
}

const PLACE_TITLE: Record<FilesPlace, string> = {
  library: "Library",
  desktop: "Desktop",
  archive: "Archive",
};

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
  archivedFolders,
  deskFolders,
  deskFileArtifactIds,
  deskIndexByArtifactId,
  desksWithItems,
  filter,
  setFilter,
  selectedId,
  onSelect,
  linkByFileId,
  folderLinks,
  confirmBeforeArchive,
  askContext,
  tasks,
  uploadFiles,
  uploadTree,
  uploads,
}: {
  list: LiveView<ArtifactRow> | null;
  artifacts: LiveCollectionHandle<Row>;
  foldersState: LiveState;
  tree: FolderTree;
  content: ArtifactRow[];
  /** Archived folders, flat and alphabetical -- the Archive place's children. */
  archivedFolders: FolderRow[];
  /** Folder shortcuts on any desk -- the Desktop place's children. */
  deskFolders: DeskFolderShortcut[];
  /** Artifact ids of loose desk file icons -- the Desktop root's population. */
  deskFileArtifactIds: ReadonlySet<string>;
  /** Which desk (by order) holds each artifact, for the "Desk N" chip. */
  deskIndexByArtifactId: Map<string, number>;
  /** How many desks hold items -- the chip renders only past one. */
  desksWithItems: number;
  filter: FilesFilter;
  setFilter: (next: FilesFilter) => void;
  selectedId: string;
  onSelect: (id: string) => void;
  /** Origin link state by backing file id (epic memql#4783). */
  linkByFileId: Map<string, LinkState | "">;
  /** The worst link state anywhere beneath each folder -- the rail's badge. */
  folderLinks: Map<string, LinkState>;
  confirmBeforeArchive: boolean;
  askContext: (tag: string) => void;
  tasks: UploadTask[];
  uploadFiles: UploadTasksApi["uploadFiles"];
  uploadTree: UploadTasksApi["uploadTree"];
  /** The provider itself, for the inspector's new-version action (memql#4806).
   *  Deliberately not routed through `uploadFiles`: that creates a placeholder
   *  ROW in this list, and a new version of an existing artifact must not add
   *  a second row to the list it is proving it does not disturb. */
  uploads: UploadProvider;
}) {
  const { presence } = useMachines();
  const { actions } = useOs();
  const connection = useOsConnection();
  const patch = (p: Partial<FilesFilter>) => setFilter({ ...filter, ...p });
  const pickRef = useRef<HTMLInputElement | null>(null);
  const rootRef = useRef<HTMLDivElement | null>(null);
  // A refused DROP (over the file or depth bound) renders here, in surface,
  // with the walker's own sentence.
  const [dropRefusal, setDropRefusal] = useState("");

  // The rail's folder actions: a context menu per node, an inline rename,
  // and the archive flow -- confirm naming the LIVE count, then the
  // children-first walk with in-surface progress (design B5/D11).
  const [folderMenu, setFolderMenu] = useState<{ x: number; y: number; node: TreeNode } | null>(null);
  // The Archive place's folder menu: Restore is the only verb an archived
  // folder offers here (epic memql#4842, #4846).
  const [archivedFolderMenu, setArchivedFolderMenu] = useState<{
    x: number;
    y: number;
    folder: FolderRow;
  } | null>(null);
  // The file row's own menu (memql#4784 AC). A THIRD entry point onto the one
  // archive action the inspector already carries -- not a second flow: the
  // same mutation, the same confirm setting, the same in-surface refusal. The
  // AC names a right-click because that is the gesture people reach for on a
  // row, and it had no menu at all.
  const [rowMenu, setRowMenu] = useState<{ x: number; y: number; row: ArtifactRow } | null>(null);
  const [rowArchive, setRowArchive] = useState<ArtifactRow | null>(null);
  const [rowNote, setRowNote] = useState("");

  const archiveOneRow = async (row: ArtifactRow) => {
    const query = connection?.query ?? null;
    if (query === null) {
      setRowNote("Not connected to the cluster, so nothing was archived.");
      return;
    }
    setRowArchive(null);
    setRowNote("");
    try {
      // Nothing is patched locally: the archive broadcasts, the row leaves
      // this list on the same feed, and it arrives in the Bin.
      await query.archiveArtifact({ artifactId: row.id });
    } catch (err: unknown) {
      setRowNote(err instanceof Error ? err.message : String(err));
    }
  };

  // Restore, from the Archive place's row menu (epic memql#4842, #4846): the
  // Bin's client-driven pair, verbatim, so the two archive surfaces cannot
  // drift apart on what "putting back" means -- plus the #4846 addition: a
  // file whose folder is not live re-files to the Library root, because a
  // row restored into an invisible folder is invisible everywhere except
  // search.
  const restoreOneRow = async (row: ArtifactRow) => {
    const query = connection?.query ?? null;
    if (query === null) {
      setRowNote("Not connected to the cluster, so nothing was restored.");
      return;
    }
    setRowNote("");
    try {
      await runRestore(planRestore(binItemFromArtifact(row)), {
        restoreArtifact: async (artifactId) => {
          await query.restoreArtifact({ artifactId });
        },
        restoreFile: async (fileId) => {
          await query.restoreLibraryFile({ fileId });
        },
        restoreFolder: async (folderId) => {
          await query.restoreLibraryFolder({ folderId });
        },
      });
      if (row.folderId !== "" && !tree.byId.has(row.folderId)) {
        await query.moveArtifactToFolder({ artifactId: row.id, folderId: "" });
      }
    } catch (err: unknown) {
      setRowNote(err instanceof Error ? err.message : String(err));
    }
  };

  // Restoring an archived FOLDER restores the folder row alone -- the Bin's
  // semantics, stated in its own note: anything inside stays archived until
  // restored itself, each item its own decision.
  const restoreFolderRow = async (folderId: string) => {
    const query = connection?.query ?? null;
    if (query === null) {
      setArchiveError("Not connected to the cluster, so nothing was restored.");
      return;
    }
    setArchiveError("");
    try {
      await query.restoreLibraryFolder({ folderId });
      if (filter.place === "archive" && filter.folderId === folderId) patch({ folderId: "" });
    } catch (err: unknown) {
      setArchiveError(err instanceof Error ? err.message : String(err));
    }
  };
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
  // being looked at, so the root takes them -- and the Archive place is
  // nowhere to upload TO, so it hands them to the Library root.
  const destinationFolderId =
    filter.search.trim() !== "" || filter.place === "archive" ? "" : (filter.folderId ?? "");

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
  const accountOptions = useAccountOptions();

  // Direct, non-archived counts by folder -- what a person would count.
  const counts = new Map<string, number>();
  // ...and archived rows counted the same way for the Archive place.
  const archivedCounts = new Map<string, number>();
  let archivedTotal = 0;
  for (const row of content) {
    if (row.archived) {
      archivedTotal += 1;
      archivedCounts.set(row.folderId, (archivedCounts.get(row.folderId) ?? 0) + 1);
      continue;
    }
    counts.set(row.folderId, (counts.get(row.folderId) ?? 0) + 1);
  }
  const deskFileCount = content.filter(
    (r) => !r.archived && deskFileArtifactIds.has(r.id),
  ).length;

  const folderNameOf = (folderId: string): string => {
    if (folderId === "") return "Library";
    return (
      tree.byId.get(folderId)?.folder.name ??
      archivedFolders.find((f) => f.id === folderId)?.name ??
      // A desk shortcut knows its folder's name even before the folders feed
      // has answered -- the double-click-from-the-desk case.
      deskFolders.find((f) => f.folderId === folderId)?.name ??
      folderId
    );
  };

  // Empty and filtered-to-empty are DIFFERENT answers: one is about the
  // place, the other about the question just asked of it.
  const narrowed =
    searching ||
    filter.kind !== "all" ||
    filter.source !== "all" ||
    filter.accountId !== ACCOUNT_ANY ||
    filter.folderId !== "";
  // The Head's meta is what the LIST is showing right now -- scoped folder,
  // facets and search included -- never a different population's number
  // beside the scope's name.
  const listedCount = list?.snapshot.rows.length ?? 0;
  const emptyText = narrowed
    ? "Nothing matches. Clear the search or filters to see your files."
    : filter.place === "desktop"
      ? "Nothing on your desks. Send a file to the desk from the Library, or drop one onto the desk."
      : filter.place === "archive"
        ? "Nothing archived. Archiving from the Library keeps files here, not deleted."
        : "Nothing in your Library yet. Drop a file onto the desk or upload one here.";

  // The Head names the scope ONCE (DESIGN.md rule 7): the place, or the
  // folder being looked at inside it.
  const headTitle =
    !searching && filter.folderId !== "" && filter.folderId !== null
      ? folderNameOf(filter.folderId)
      : PLACE_TITLE[filter.place];

  // The Refine chips: every active facet, removable in place (rule 2).
  const refineChips: RefineChip[] = [];
  if (filter.kind !== "all") {
    refineChips.push({
      id: "kind",
      label: KIND_TABS.find((t) => t.value === filter.kind)?.label ?? filter.kind,
      onRemove: () => patch({ kind: "all" }),
    });
  }
  if (filter.source !== "all") {
    refineChips.push({ id: "source", label: filter.source, onRemove: () => patch({ source: "all" }) });
  }
  if (filter.accountId !== ACCOUNT_ANY) {
    const account = accountOptions.find((a) => a.id === filter.accountId);
    refineChips.push({
      id: "client",
      label: filter.accountId === ACCOUNT_NONE ? "No client" : account ? accountName(account) : "Client",
      onRemove: () => patch({ accountId: ACCOUNT_ANY }),
    });
  }

  return (
    <div
      ref={rootRef}
      className="os-files"
      onDragOver={(event) => {
        if (!event.dataTransfer.types.includes("Files")) return;
        event.preventDefault();
        event.stopPropagation();
      }}
      onDrop={onDrop}
    >
      {/* THE HEAD IS THE WHOLE TOP (DESIGN.md rules 1-3): the scope's name
          and count, the quiet sort, one Refine affordance, the Upload
          primary. The nine-control strip this replaces is the reason rule 2
          exists. */}
      <Head title={headTitle} meta={listedCount}>
        <SortControl
          ascending={filter.sortAscending}
          onToggle={() => patch({ sortAscending: !filter.sortAscending })}
        />
        <Refine
          search={filter.search}
          onSearch={(search) => patch({ search })}
          chips={refineChips}
          label="Refine files"
        >
          <div className="os-files-kinds" role="radiogroup" aria-label="Kind">
            {KIND_TABS.map((tab) => (
              <button
                key={tab.value}
                type="button"
                role="radio"
                aria-checked={filter.kind === tab.value}
                className="os-files-kind"
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
          {/* THE CLIENT FILTER (epic memql#4800, D5). Client-side over the
              seeded snapshot like every other facet here -- a row promoted
              before `accountIds` existed has no key at all, and only the fold
              reads absence and the empty list as one answer. "No client" is a
              first-class option because "what still needs filing" is the one
              question the other two cannot express. */}
          <Select
            id="files-account"
            label="Client"
            value={filter.accountId}
            onChange={(accountId) => patch({ accountId })}
          >
            <option value={ACCOUNT_ANY}>Any client</option>
            <option value={ACCOUNT_NONE}>No client</option>
            {accountOptions.map((account) => (
              <option key={account.id} value={account.id}>
                {accountIsArchived(account) ? `${accountName(account)} (archived)` : accountName(account)}
              </option>
            ))}
          </Select>
        </Refine>
        <Button tone="primary" onClick={() => pickRef.current?.click()}>
          <Upload size={13} aria-hidden /> Upload
        </Button>
      </Head>
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

      <div className="os-files-body">
        {/* THE RAIL'S THREE PLACES (epic memql#4842, #4846): Library is what
            you have, Desktop is what sits on your desks, Archive is what you
            archived. Only items PLACED on a desk appear under Desktop; only
            archived rows under Archive -- the old show-archived checkbox is
            what this replaces. */}
        <nav className="os-files-rail" aria-label="Places and folders">
          <button
            type="button"
            className="os-files-node"
            data-current={!searching && filter.place === "library" && filter.folderId === "" ? true : undefined}
            onClick={() => patch({ place: "library", folderId: "", search: "" })}
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
              folderLinks={folderLinks}
              currentId={searching || filter.place !== "library" ? null : filter.folderId}
              renamingFolderId={renamingFolderId}
              onScope={(folderId) => patch({ place: "library", folderId, search: "" })}
              onMenu={(x, y, menuNode) => {
                // The menu positions inside THIS box, so viewport coords
                // become box coords -- against the window frame they would
                // open a window-offset away from the click.
                const rect = rootRef.current?.getBoundingClientRect();
                setFolderMenu({
                  x: x - (rect?.left ?? 0),
                  y: y - (rect?.top ?? 0),
                  node: menuNode,
                });
              }}
              onRename={(folderId, name) => void renameFolder(folderId, name)}
              onCancelRename={() => setRenamingFolderId("")}
            />
          ))}
          {/* The one rail ACTION (rule 6), beside the block it acts on: it
              creates a folder in the Library scope being looked at. Below the
              archived flood it would be furniture nobody finds. */}
          <button
            type="button"
            className="os-files-node"
            data-action
            disabled={connection === null}
            onClick={() =>
              void newFolderIn(searching || filter.place !== "library" ? "" : (filter.folderId ?? ""))
            }
          >
            <FolderPlus size={14} aria-hidden />
            <span className="os-files-node-name">New folder</span>
          </button>

          <button
            type="button"
            className="os-files-node os-files-place"
            data-current={!searching && filter.place === "desktop" && filter.folderId === "" ? true : undefined}
            onClick={() => patch({ place: "desktop", folderId: "", search: "" })}
          >
            <Monitor size={14} aria-hidden />
            <span className="os-files-node-name">Desktop</span>
            <span className="os-files-node-count">{deskFileCount > 0 ? deskFileCount : ""}</span>
          </button>
          {deskFolders.map((shortcut) => (
            <button
              key={shortcut.folderId}
              type="button"
              className="os-files-node"
              style={{ paddingInlineStart: "24px" }}
              data-current={
                !searching && filter.place === "desktop" && filter.folderId === shortcut.folderId
                  ? true
                  : undefined
              }
              onClick={() => patch({ place: "desktop", folderId: shortcut.folderId, search: "" })}
            >
              <Folder size={14} aria-hidden />
              <span className="os-files-node-name">{folderNameOf(shortcut.folderId)}</span>
              <span className="os-files-node-count">
                {(counts.get(shortcut.folderId) ?? 0) > 0 ? counts.get(shortcut.folderId) : ""}
              </span>
            </button>
          ))}

          <button
            type="button"
            className="os-files-node os-files-place"
            data-current={!searching && filter.place === "archive" && filter.folderId === "" ? true : undefined}
            onClick={() => patch({ place: "archive", folderId: "", search: "" })}
          >
            <ArchiveGlyph size={14} aria-hidden />
            <span className="os-files-node-name">Archive</span>
            <span className="os-files-node-count">{archivedTotal > 0 ? archivedTotal : ""}</span>
          </button>
          {archivedFolders.map((folder) => (
            <button
              key={folder.id}
              type="button"
              className="os-files-node"
              style={{ paddingInlineStart: "24px" }}
              data-current={
                !searching && filter.place === "archive" && filter.folderId === folder.id
                  ? true
                  : undefined
              }
              onClick={() => patch({ place: "archive", folderId: folder.id, search: "" })}
              onContextMenu={(event) => {
                event.preventDefault();
                event.stopPropagation();
                const rect = rootRef.current?.getBoundingClientRect();
                setArchivedFolderMenu({
                  x: event.clientX - (rect?.left ?? 0),
                  y: event.clientY - (rect?.top ?? 0),
                  folder,
                });
              }}
            >
              <Folder size={14} aria-hidden />
              <span className="os-files-node-name">{folder.name}</span>
              <span className="os-files-node-count">
                {(archivedCounts.get(folder.id) ?? 0) > 0 ? archivedCounts.get(folder.id) : ""}
              </span>
            </button>
          ))}

          {railNote !== "" ? <Caption>{railNote}</Caption> : null}
          {foldersState === "degraded" || foldersState === "disconnected" ? (
            <Caption>Folder updates are behind -- showing the last known tree.</Caption>
          ) : null}
        </nav>

        <div className="os-files-list">
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
              next="Everything inside archives too, children first. Nothing is deleted -- it all lives under Archive, where Restore puts it back."
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
            key={`${filter.place}|${filter.folderId ?? "~"}|${filter.kind}|${filter.source}|${filter.accountId}|${filter.search}`}
            source={list}
            rowId={(r) => r.id}
            fingerprint={artifactFingerprint}
            label={headTitle}
            emptyText={emptyText}
            renderRow={(row, tick) => (
              <FileLine
                row={row}
                tick={tick}
                searching={searching}
                folderNameOf={folderNameOf}
                presence={presence}
                deskIndex={
                  filter.place === "desktop" && desksWithItems > 1
                    ? (deskIndexByArtifactId.get(row.id) ?? null)
                    : null
                }
                linkState={
                  row.kind === "file"
                    ? (linkByFileId.get(row.sourceConceptRef.split(":").pop() ?? "") ?? "")
                    : ""
                }
                open={selectedId === row.id}
                onToggle={() => onSelect(selectedId === row.id ? "" : row.id)}
                onMenu={(x, y) => setRowMenu({ x, y, row })}
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
            uploads={uploads}
            onAsk={askContext}
            onClose={() => onSelect("")}
          />
        )}
      </div>

      {rowMenu !== null ? (
        <ContextMenu
          x={rowMenu.x}
          y={rowMenu.y}
          label="File"
          entries={
            rowMenu.row.archived
              ? [
                  {
                    id: "restore",
                    // The Bin's verb, kept verbatim (cohesion): one name for
                    // one action wherever it appears.
                    label: "Restore",
                    disabled: connection === null,
                    onSelect: () => void restoreOneRow(rowMenu.row),
                  },
                ]
              : [
                  {
                    id: "archive",
                    // "Move to Bin" rather than "Delete": the action's name has
                    // to be what it DOES, and nothing here deletes. It keeps
                    // that name through the whole flow, which is why the
                    // confirm below says the same and the Bin says the item is
                    // in it.
                    label: "Move to Bin",
                    disabled: connection === null,
                    onSelect: () =>
                      confirmBeforeArchive
                        ? setRowArchive(rowMenu.row)
                        : void archiveOneRow(rowMenu.row),
                  },
                ]
          }
          onClose={() => setRowMenu(null)}
        />
      ) : null}
      {archivedFolderMenu !== null ? (
        <ContextMenu
          x={archivedFolderMenu.x}
          y={archivedFolderMenu.y}
          label="Archived folder"
          entries={[
            {
              id: "restore",
              label: "Restore",
              disabled: connection === null,
              onSelect: () => void restoreFolderRow(archivedFolderMenu.folder.id),
            },
          ]}
          onClose={() => setArchivedFolderMenu(null)}
        />
      ) : null}
      {rowArchive !== null ? (
        <Notice
          tone="warn"
          sentence={`Move "${artifactName(rowArchive)}" to the Bin?`}
          next="Nothing is deleted -- it keeps its bytes, its history and everywhere it came from, and the Bin can put it back."
        >
          <div className="os-files-confirm">
            <Button tone="danger" onClick={() => void archiveOneRow(rowArchive)}>
              Move to Bin
            </Button>
            <Button onClick={() => setRowArchive(null)}>Cancel</Button>
          </div>
        </Notice>
      ) : null}
      {rowNote !== "" ? (
        <Notice
          tone="error"
          sentence="The archive was refused."
          next="The file is where it was."
          detail={rowNote}
        />
      ) : null}
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
  folderLinks,
  currentId,
  renamingFolderId,
  onScope,
  onMenu,
  onRename,
  onCancelRename,
}: {
  node: TreeNode;
  counts: Map<string, number>;
  /** The WORST origin link state anywhere beneath this folder (epic
   *  memql#4783), or absent -- which is most folders, and draws nothing. */
  folderLinks: Map<string, LinkState>;
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
          {/* THE ROLLUP DOT, and it is a dot rather than a count on purpose:
              the reason to mark a folder is to make somebody open it, and a
              folder holding one missing file needs opening exactly as much as
              one holding forty. `synced` draws nothing at all -- a green mark
              on every backed-up folder is noise that makes the few that need
              attention invisible. */}
          {(() => {
            const rollup = folderLinks.get(node.folder.id);
            if (rollup === undefined || rollup === "synced") return null;
            return (
              <span
                className="os-files-node-link"
                data-link={rollup}
                title={`Something in here: ${LINK_SENTENCE[rollup]}`}
                role="img"
                aria-label={`Something in this folder is ${LINK_LABEL[rollup]}`}
              />
            );
          })()}
          <span className="os-files-node-count">{count > 0 ? count : ""}</span>
        </button>
      )}
      {node.children.map((child) => (
        <RailNode
          key={child.folder.id}
          node={child}
          folderLinks={folderLinks}
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
  deskIndex,
  linkState,
  open,
  onToggle,
  onMenu,
}: {
  row: ArtifactRow;
  tick: "added" | "updated" | null;
  searching: boolean;
  folderNameOf: (folderId: string) => string;
  presence: (workerId: string) => { name?: string; online: boolean } | null;
  /** Which desk holds this row (0-based), when more than one desk holds
   *  items; null renders nothing. */
  deskIndex: number | null;
  /** The origin link state (epic memql#4783), or "" for a file with no origin
   *  to link to -- which is most of them, and renders nothing. */
  linkState: LinkState | "";
  open: boolean;
  onToggle: () => void;
  onMenu: (x: number, y: number) => void;
}) {
  const story = fileStory(row, row.producedByWorkerId ? presence(row.producedByWorkerId) : null);
  const extraLabels = row.labels.length > 2 ? row.labels.length - 2 : 0;
  // DRAGGABLE TO THE BIN (memql#4784). The payload travels with the drag,
  // because the dock holds no Library feed of its own.
  //
  // The listeners go on a WRAPPER rather than on the row button, and dnd-kit's
  // `attributes` are deliberately not spread: they set role="button" and a
  // tabIndex, which on a div wrapping a button is a second interactive element
  // announcing the same thing. That costs the keyboard drag, which is the
  // right trade here -- archiving already has two keyboard-reachable routes
  // (the inspector's Archive button and the row's own action), and neither of
  // them is a drag.
  const draggable = useDraggable({
    id: `artifact:${row.id}`,
    data: {
      artifactId: row.id,
      name: artifactName(row),
      folder: false,
      deskItemId: "",
    } satisfies BinDropPayload,
  });
  return (
    <div
      ref={draggable.setNodeRef}
      className="os-files-line"
      data-dragging={draggable.isDragging || undefined}
      onContextMenu={(event) => {
        // The shell's right-click rule: a surface with its own menu says so,
        // and stops the root handler from re-enabling the browser's.
        event.preventDefault();
        event.stopPropagation();
        onMenu(event.clientX, event.clientY);
      }}
      {...draggable.listeners}
    >
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
      {deskIndex !== null ? <Chip tone="muted">Desk {deskIndex + 1}</Chip> : null}
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
      {linkState === "" ? null : (
        <Chip
          tone={linkState === "synced" ? "neutral" : "accent"}
          title={LINK_SENTENCE[linkState]}
        >
          {LINK_LABEL[linkState]}
        </Chip>
      )}
    </ListRow>
    </div>
  );
}
