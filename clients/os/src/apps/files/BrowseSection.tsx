import { useEffect, useRef, useState } from "react";
import { ChevronDown, FolderPlus, Plus, Upload } from "lucide-react";
import {
  newShortId,
  rowNumber,
  rowString,
  type LiveState,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import { useAuthSource } from "../../auth/context";
import { useSession } from "../../chrome/access";
import { ContextMenu } from "../../chrome/ContextMenu";
import { useOs } from "../../chrome/state";
import { openInVsCode, VSCODE_NO_ANSWER_MESSAGE } from "../../items/vscode";
import { downloadArtifact } from "./actions/download";
import { useOsConnection } from "../../live/connection";
import { entriesOf, hasDirectory, walkEntries } from "../../items/folderDrop";
import { planArchive, runArchiveWalk, subtreeHoldsArtifact } from "./actions/archive";
import { foldBinRail } from "./fold";
import { kindGlyph } from "./glyphs";
import { Rail, type ExpandedPlaces } from "./Rail";
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
// The rail offers three PLACES -- Library, Desktop, Bin -- and the top of
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
  bin: "Bin",
};

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
  expanded,
  setExpanded,
  openBinFolders,
  setOpenBinFolders,
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
  /** Archived folders, flat and alphabetical -- the Bin place's folders. */
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
  /** Which rail places are open. Held by the app root so switching to
   *  Settings and back does not shut everything the person just opened. */
  expanded: ExpandedPlaces;
  setExpanded: (next: ExpandedPlaces) => void;
  /** Which of the Bin's own disclosures are open. Held at the app root for
   *  the reason `expanded` is; the setter is functional-only, and Rail.tsx
   *  records what a value-taking one costs inside a React batch. */
  openBinFolders: ReadonlySet<string>;
  setOpenBinFolders: (update: (prev: ReadonlySet<string>) => ReadonlySet<string>) => void;
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
  const { config } = useSession();
  const authSource = useAuthSource();
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
  // The Bin place's folder menu: Restore is the only verb an archived
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
  // The row actions' one refusal slot, carrying the SENTENCE with it. Four
  // verbs report here (archive, move, download, new version) and a single
  // shared wording would tell the reader neither which one failed nor what
  // state their file is now in.
  const [rowNote, setRowNote] = useState<{ sentence: string; next: string; detail: string } | null>(
    null,
  );
  const refuse = (sentence: string, next: string, err: unknown) =>
    setRowNote({ sentence, next, detail: err instanceof Error ? err.message : String(err) });
  // The row menu's own flows. Each renders beside the list, in surface, the
  // way every other refusal in this app does.
  const [rowMove, setRowMove] = useState<ArtifactRow | null>(null);
  const [rowVersionFor, setRowVersionFor] = useState<ArtifactRow | null>(null);
  const [vsNoAnswer, setVsNoAnswer] = useState(false);
  const versionPick = useRef<HTMLInputElement | null>(null);

  // Every heavy verb the row menu offers is the inspector's verb, reached by
  // the SAME function -- the download decision is shared in actions/download,
  // the desk hand-off is one shell action, the new version is the one upload
  // provider. A right-click is a third entry point onto one set of actions,
  // never a second implementation of them.
  const downloadRow = async (row: ArtifactRow) => {
    setRowNote(null);
    try {
      await downloadArtifact({
        artifactId: row.id,
        name: artifactName(row),
        fileId: row.kind === "file" ? (row.sourceConceptRef.split(":").pop() ?? "") : "",
        readFile: connection
          ? async (id) => {
              const result = await connection.query.libraryFileById({ fileId: id });
              const fileRow = result.rows()[0] ?? null;
              if (!fileRow) return null;
              return { sizeBytes: rowNumber(fileRow, "size"), name: rowString(fileRow, "name") };
            }
          : null,
        bearer: () => authSource.bearer(),
      });
    } catch (err: unknown) {
      refuse("The download did not land.", "Nothing was saved.", err);
    }
  };

  const sendRowToDesk = (row: ArtifactRow) => {
    const outcome = actions.sendFileToDesk({
      artifactId: row.id,
      title: artifactName(row),
      fileKind: row.kind,
      source: row.source,
      ...(row.producedByWorkerId ? { producedByWorkerId: row.producedByWorkerId } : {}),
    });
    setRailNote(
      outcome === "full"
        ? "The desk is full -- remove something from it first."
        : outcome === "focused"
          ? "Already on the desk; it is selected there now."
          : "On the desk.",
    );
    setTimeout(() => setRailNote(""), 5000);
  };

  const moveRowTo = async (row: ArtifactRow, folderId: string) => {
    const query = connection?.query ?? null;
    setRowMove(null);
    if (query === null) {
      refuse("The move was refused.", "The file is where it was.", "Not connected to the cluster.");
      return;
    }
    setRowNote(null);
    try {
      await query.moveArtifactToFolder({ artifactId: row.id, folderId });
    } catch (err: unknown) {
      refuse("The move was refused.", "The file is where it was.", err);
    }
  };

  const sendNewVersion = async (row: ArtifactRow, file: File) => {
    setRowNote(null);
    try {
      await uploads.upload(file, { targetArtifactId: row.id }).done;
    } catch (err: unknown) {
      refuse("The new version was not accepted.", "This file still holds the version it had.", err);
    }
  };

  const archiveOneRow = async (row: ArtifactRow) => {
    const query = connection?.query ?? null;
    if (query === null) {
      refuse("The archive was refused.", "The file is where it was.", "Not connected to the cluster.");
      return;
    }
    setRowArchive(null);
    setRowNote(null);
    try {
      // Nothing is patched locally: the archive broadcasts, the row leaves
      // this list on the same feed, and it arrives in the Bin.
      await query.archiveArtifact({ artifactId: row.id });
    } catch (err: unknown) {
      refuse("The archive was refused.", "The file is where it was.", err);
    }
  };

  // Restore, from the Bin place's row menu (epic memql#4842, #4846): the
  // Bin's client-driven pair, verbatim, so the two archive surfaces cannot
  // drift apart on what "putting back" means -- plus the #4846 addition: a
  // file whose folder is KNOWN-ARCHIVED re-files to the Library root, because
  // a row restored into an invisible folder is invisible everywhere except
  // search. Membership in the archived list is the predicate, NOT absence
  // from the live tree: while the folders feed is still seeding the tree is
  // empty, and an absence test would re-file rows out of live folders.
  const restoreOneRow = async (row: ArtifactRow) => {
    const query = connection?.query ?? null;
    if (query === null) {
      refuse("The restore was refused.", "The file is still archived.", "Not connected to the cluster.");
      return;
    }
    setRowNote(null);
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
      if (row.folderId !== "" && archivedFolders.some((f) => f.id === row.folderId)) {
        await query.moveArtifactToFolder({ artifactId: row.id, folderId: "" });
      }
    } catch (err: unknown) {
      refuse("The restore was refused.", "The file is still archived.", err);
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
      if (filter.place === "bin" && filter.folderId === folderId) patch({ folderId: "" });
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
        // A folder with no file anywhere beneath it is DELETED rather than
        // archived: there is nothing in it to put back, so a row in the Bin
        // beside the things that ARE waiting there would be noise. Still a
        // soft delete -- the row survives, every folder read excludes it.
        deleteFolder: async (id) => {
          await query.deleteLibraryFolder({ folderId: id });
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
  // being looked at, so the root takes them -- and the Bin is nowhere to
  // upload TO, so it hands them to the Library root.
  const destinationFolderId =
    filter.search.trim() !== "" || filter.place === "bin" ? "" : (filter.folderId ?? "");

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

  const rowMenuEntries = (row: ArtifactRow) => {
    const ask = {
      id: "ask",
      label: "Ask about this file",
      onSelect: () => askContext(`app:files/browse file:${artifactName(row)}`),
    };
    const download = {
      id: "download",
      label: "Download",
      onSelect: () => void downloadRow(row),
    };
    if (row.archived) {
      return [
        {
          id: "restore",
          // The Bin's verb, kept verbatim (cohesion): one name for one action
          // wherever it appears.
          label: "Restore",
          disabled: connection === null,
          onSelect: () => void restoreOneRow(row),
        },
        download,
        ask,
      ];
    }
    return [
      { id: "open", label: "Open in VS Code", onSelect: () => openVsCodeFor(row) },
      { id: "desk", label: "Send to desktop", onSelect: () => sendRowToDesk(row) },
      download,
      ...(row.kind === "file"
        ? [
            {
              id: "version",
              label: "Upload new version",
              onSelect: () => {
                setRowVersionFor(row);
                versionPick.current?.click();
              },
            },
          ]
        : []),
      {
        id: "move",
        label: "Move to folder",
        disabled: connection === null,
        onSelect: () => setRowMove(row),
      },
      ask,
      {
        id: "archive",
        // "Move to Bin" rather than "Delete": the action's name has to be what
        // it DOES, and nothing here deletes. It keeps that name through the
        // whole flow, which is why the confirm below says the same and the Bin
        // says the item is in it.
        label: "Move to Bin",
        disabled: connection === null,
        onSelect: () =>
          confirmBeforeArchive ? setRowArchive(row) : void archiveOneRow(row),
      },
    ];
  };

  const openVsCodeFor = (row: ArtifactRow) => {
    setVsNoAnswer(false);
    openInVsCode(config.domain, row.id, () => setVsNoAnswer(true));
  };

  const selected = content.find((r) => r.id === selectedId) ?? null;
  const archivedFolderIdSet = new Set(archivedFolders.map((f) => f.id));
  const searching = filter.search.trim() !== "";
  const accountOptions = useAccountOptions();

  // Direct, non-archived counts by folder -- what a person would count.
  const counts = new Map<string, number>();
  const archivedFiles: ArtifactRow[] = [];
  for (const row of content) {
    if (row.archived) {
      archivedFiles.push(row);
      continue;
    }
    counts.set(row.folderId, (counts.get(row.folderId) ?? 0) + 1);
  }
  // The Bin's picture, from the same two populations the Bin app reads: the
  // archived index rows, and the archived folders. ONE FOLD, so the rail's
  // per-folder numbers and the Bin app's list cannot drift apart -- they used
  // to be two counting loops that happened to agree.
  const bin = foldBinRail(archivedFiles, archivedFolders);
  // Clicking a file in the Bin's rail means "show me that", which is two
  // things at once: scope the list so the row is IN it, then select it so the
  // inspector opens on it. The rail hands back the scope it found the file
  // under -- "" for a loose file, which in the Bin already means everything
  // archived rather than a root folder nothing is filed in.
  const showBinFile = (row: ArtifactRow, folderId: string) => {
    patch({ place: "bin", folderId, search: "" });
    setExpanded({ ...expanded, bin: true });
    onSelect(row.id);
  };

  const deskFileCount = content.filter(
    (r) => !r.archived && deskFileArtifactIds.has(r.id),
  ).length;
  // The COLLAPSED Library's number is the whole place, not its root folder.
  // A shut summary reading "3" over two hundred files is a smaller number
  // standing in front of a bigger one it does not mention; the per-folder
  // counts above stay direct, which is the recorded answer for a location.
  const libraryTotal = content.length - archivedFiles.length;

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
      : filter.place === "bin"
        ? "The Bin is empty. Archiving from the Library keeps files here, not deleted."
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
        {/* ONE WAY TO ADD SOMETHING (DESIGN.md rule 1: at most one primary
            action). Uploading a file and making a folder are the same
            question -- "put something here" -- and they were answered in two
            places: a primary button up here and a rail ACTION wedged between
            the Library tree and the Desktop place, where it read as a folder
            you could open. The rail action is gone; both answers live on this
            button, and both land in the folder currently being looked at. */}
        <AddMenu
          disabled={connection === null}
          destinationName={destinationFolderId === "" ? "Library" : folderNameOf(destinationFolderId)}
          onUpload={() => pickRef.current?.click()}
          onNewFolder={() => void newFolderIn(destinationFolderId)}
        />
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
        {/* THE RAIL (epic memql#4842, #4846; collapsed by default here).
            Library is what you have, Desktop is what sits on your desks, the
            Bin is what you archived. Each place is a disclosure over its own
            contents -- see Rail.tsx for why they start shut, why selecting
            one also opens it, and why the Bin is the one place whose rail
            reaches files as well as folders. */}
        <Rail
          filter={filter}
          patch={patch}
          tree={tree}
          counts={counts}
          folderLinks={folderLinks}
          bin={bin}
          openBinFolders={openBinFolders}
          setOpenBinFolders={setOpenBinFolders}
          deskFolders={deskFolders}
          folderNameOf={folderNameOf}
          libraryTotal={libraryTotal}
          deskFileCount={deskFileCount}
          expanded={expanded}
          setExpanded={setExpanded}
          renamingFolderId={renamingFolderId}
          onRename={(folderId, name) => void renameFolder(folderId, name)}
          onCancelRename={() => setRenamingFolderId("")}
          onFolderMenu={(x, y, node) => setFolderMenu({ x, y, node })}
          onArchivedFolderMenu={(x, y, folder) =>
            setArchivedFolderMenu({ x, y, folder })
          }
          onSelectBinFile={showBinFile}
          selectedFileId={selectedId}
          railNote={railNote}
          foldersState={foldersState}
        />

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
          {/* THE CONFIRM NAMES THE DISPOSITION IT IS ABOUT TO PERFORM, and
              the button keeps that name through the flow. A folder holding no
              file at any depth is deleted rather than archived, so offering
              to "archive" it and then removing it from every surface would be
              the interface saying one thing and doing another -- on the one
              action in this app that a person cannot undo by clicking
              Restore. */}
          {pendingArchive !== "" ? (
            (() => {
              const plan = planArchive(tree, content, pendingArchive);
              const name = folderNameOf(pendingArchive);
              const keeping = subtreeHoldsArtifact(tree, content, pendingArchive);
              const emptied = plan.deleteFolderIds.length;
              return (
                <Notice
                  tone="warn"
                  sentence={
                    keeping
                      ? `Archive "${name}" and its ${plan.itemCount} items?`
                      : `Delete "${name}"?`
                  }
                  next={
                    keeping
                      ? `Everything inside archives too, children first, and lives under Archive where Restore puts it back.${
                          emptied > 0
                            ? ` The ${emptied} empty ${emptied === 1 ? "folder" : "folders"} inside are deleted instead -- there is nothing in them to put back.`
                            : ""
                        }`
                      : `It holds no files at any depth${
                          emptied > 1 ? `, and nor do the ${emptied - 1} folders inside it` : ""
                        }. There is nothing to put back, so it is removed from every surface rather than kept in the Bin.`
                  }
                >
                  <div className="os-files-confirm">
                    <Button tone="danger" onClick={() => void runFolderArchive(pendingArchive)}>
                      {keeping ? "Archive" : "Delete"}
                    </Button>
                    <Button onClick={() => setPendingArchive("")}>Cancel</Button>
                  </div>
                </Notice>
              );
            })()
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
            archivedFolderIds={archivedFolderIdSet}
            presence={presence}
            confirmBeforeArchive={confirmBeforeArchive}
            uploads={uploads}
            onAsk={askContext}
            onClose={() => onSelect("")}
          />
        )}
      </div>

      {/* THE ROW'S MENU IS THE INSPECTOR'S ACTION SET (memql#4860 wave).
          It used to hold one entry -- Move to Bin -- which made a right-click
          look like it had failed to load rather than like the surface it is.
          Every verb here reaches the same function the inspector's button
          does; the two heavy ones (download, new version) run the shared
          implementations rather than second copies. */}
      {rowMenu !== null ? (
        <ContextMenu
          x={rowMenu.x}
          y={rowMenu.y}
          label="File"
          entries={rowMenuEntries(rowMenu.row)}
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
      {rowNote !== null ? (
        <Notice
          tone="error"
          sentence={rowNote.sentence}
          next={rowNote.next}
          detail={rowNote.detail}
        >
          <Button onClick={() => setRowNote(null)}>Dismiss</Button>
        </Notice>
      ) : null}
      {vsNoAnswer ? <Notice tone="warn" sentence={VSCODE_NO_ANSWER_MESSAGE} /> : null}
      {/* MOVE, re-homed from the inspector (the owner asked for it off that
          panel). A picker rather than a submenu: the folder list is a tree of
          arbitrary size and a context menu is not where an arbitrary list
          belongs. */}
      {rowMove !== null ? (
        <Notice tone="warn" sentence={`Move "${artifactName(rowMove)}" to another folder`}>
          <div className="os-files-confirm">
            <span className="os-files-move-pick">
            <Select
              id={`files-move-${rowMove.id}`}
              label="Move to folder"
              value={rowMove.folderId}
              onChange={(folderId) => void moveRowTo(rowMove, folderId)}
            >
              <option value="">Library (top level)</option>
              {flatFolderOptions(tree).map((option) => (
                <option key={option.id} value={option.id}>
                  {option.label}
                </option>
              ))}
            </Select>
            </span>
            <Button onClick={() => setRowMove(null)}>Cancel</Button>
          </div>
        </Notice>
      ) : null}
      {/* The picker is never seen: a bare file input cannot be styled into
          this shell's button language, and every surface here uses the same
          one. Cleared on change so picking the SAME file twice fires again --
          a change event that never comes reads as a dead menu item. */}
      <input
        ref={versionPick}
        type="file"
        className="os-visually-hidden"
        aria-label="Choose a file to upload as the new version"
        onChange={(event) => {
          const file = event.target.files?.[0];
          event.target.value = "";
          const row = rowVersionFor;
          setRowVersionFor(null);
          if (file && row) void sendNewVersion(row, file);
        }}
      />
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
              // The verb the menu offers is the one that will happen. An
              // empty folder is deleted, and calling that "Archive" here and
              // "Delete" in the confirm one click later would teach somebody
              // that the two words mean the same thing in this app.
              label: subtreeHoldsArtifact(tree, content, folderMenu.node.folder.id)
                ? "Archive"
                : "Delete",
              disabled: connection === null,
              onSelect: () =>
                confirmBeforeArchive
                  ? setPendingArchive(folderMenu.node.folder.id)
                  : void runFolderArchive(folderMenu.node.folder.id),
            },
            {
              id: "new-folder",
              // Creating a folder INSIDE a named one is the placement the
              // Head's Add control cannot express -- it puts things in the
              // folder being looked at, which is not always this one.
              label: "New folder inside",
              disabled: connection === null,
              onSelect: () => void newFolderIn(folderMenu.node.folder.id),
            },
          ]}
          onClose={() => setFolderMenu(null)}
        />
      ) : null}
    </div>
  );
}

/**
 * The Add menu: one primary control, two ways to put something in the place
 * being looked at.
 *
 * IT NAMES THE DESTINATION. Both actions land in the folder currently
 * scoped, which is invisible from the button -- so the menu says where,
 * rather than leaving somebody to find out by doing it. Searching or
 * standing in Archive resolves to the Library root upstream, and the label
 * follows that same value rather than guessing at it separately.
 */
function AddMenu({
  disabled,
  destinationName,
  onUpload,
  onNewFolder,
}: {
  disabled: boolean;
  destinationName: string;
  onUpload: () => void;
  onNewFolder: () => void;
}) {
  const [open, setOpen] = useState(false);
  const wrap = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!open) return;
    const onPointer = (event: PointerEvent) => {
      if (!wrap.current?.contains(event.target as Node)) setOpen(false);
    };
    // Capture phase, like the shell's context menu: a click that lands on a
    // control which stops propagation must still shut this.
    window.addEventListener("pointerdown", onPointer, true);
    return () => window.removeEventListener("pointerdown", onPointer, true);
  }, [open]);

  const choose = (run: () => void) => {
    setOpen(false);
    run();
  };

  return (
    <div className="os-files-add" ref={wrap}>
      <Button tone="primary" ariaExpanded={open} onClick={() => setOpen(!open)}>
        <Plus size={13} aria-hidden /> Add <ChevronDown size={12} aria-hidden />
      </Button>
      {open ? (
        <div
          className="os-menu os-files-add-menu"
          role="menu"
          aria-label="Add to this folder"
          onKeyDown={(event) => {
            if (event.key === "Escape") {
              event.stopPropagation();
              setOpen(false);
            }
          }}
        >
          <p className="os-menu-head">Into {destinationName}</p>
          <button
            type="button"
            role="menuitem"
            className="os-menu-item"
            onClick={() => choose(onUpload)}
          >
            <Upload size={13} aria-hidden /> Upload files
          </button>
          <button
            type="button"
            role="menuitem"
            className="os-menu-item"
            disabled={disabled}
            onClick={() => choose(onNewFolder)}
          >
            <FolderPlus size={13} aria-hidden /> New folder
          </button>
        </div>
      ) : null}
    </div>
  );
}

/** The tree flattened for the move picker, depth as indentation. */
function flatFolderOptions(tree: FolderTree): Array<{ id: string; label: string }> {
  const out: Array<{ id: string; label: string }> = [];
  const walk = (node: TreeNode, depth: number) => {
    out.push({ id: node.folder.id, label: `${"  ".repeat(depth)}${node.folder.name}` });
    for (const child of node.children) walk(child, depth + 1);
  };
  for (const root of tree.roots) walk(root, 0);
  return out;
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
