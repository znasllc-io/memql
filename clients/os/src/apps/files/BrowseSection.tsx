import { LoadMore } from "../../kit/LoadMore";
import type { LibraryArtifactsFeed } from "./useLibrary";
import { usePageScroll } from "../../kit/pageNavigation";
import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import { ArrowUpDown, Download, FilePlus, FileText, FolderInput, FolderPlus, Import, Layers, Monitor, RotateCcw, Search, Share2, SquareArrowOutUpRight, Trash2, Upload } from "lucide-react";
import { CardCollectionSearch } from "../../kit/CardCollectionEditor";
import { IconButton } from "../../kit/IconButton";
import { Mark } from "../../chrome/Mark";
import {
  newShortId,
  rowNumber,
  rowString,
  type LiveState,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import { useAuthSource } from "../../auth/context";
import { useSession } from "../../chrome/access";
import { ContextMenu, type MenuEntry } from "../../chrome/ContextMenu";
import { useOs } from "../../chrome/state";
import { canOpen } from "../../system/registry";
import { openInVsCode, VSCODE_NO_ANSWER_MESSAGE } from "../../items/vscode";
import { downloadArtifact } from "./actions/download";
import { editorFilename, isZipArtifact } from "../../items/editorPreference";
import { useOsConnection } from "../../live/connection";
import { entriesOf, hasDirectory, walkEntries } from "../../items/folderDrop";
import { FolderSelect, folderOptions } from "../../items/FolderSelect";
import { planArchive, readArchiveMembers, runArchiveWalk, subtreeHoldsArtifact } from "./actions/archive";

import { composeIntent, MATERIALIZER_APP_ID, MATERIALIZER_COMPOSE_SECTION, MATERIALIZER_MADE_SECTION } from "../../items/materialize";
import { AddButton } from "../../kit/AddButton";
import { Submenu } from "../../kit/Submenu";
import { InPlaceAsk } from "../../kit/pressHold";
import { FileFolders } from "./FileFolders";
import { FilesRail } from "./Rail";
import { noticeFromRow, outgoingFromRow, overseenSharers, shareStands, useCanShareFiles, useSharedWithMe, useSharesOverseen, useSharing, useShareTitles, type OutgoingShare, type SharedNotice } from "./useSharing";
import { roleAdmits } from "../../system/roles";
import { AutoRefresh } from "../../kit/AutoRefresh";
import { SharingPlace } from "./SharingPlace";
import { ShareView } from "./ShareView";
import { useLiveView } from "../../live/liveView";
import { FileTile } from "./Tiles";
import {
  Button,
  Caption,
  formatBytes,
  Head,
  LiveList,
  Notice,
  listCount,
  Select,
} from "../../kit";
import type { Breadcrumb } from "../../kit/Breadcrumbs";
import { useMachinePresence } from "../../live/machines";
import type { LiveView } from "../../live/liveView";
import {
  ADDED_WITHIN,
  addedWithin,
  compareRows,
  FILE_TYPES,
  FILES_SORTS,
  fileKindOf,
  needsWholePlace,
  type AddedWithin,
  type FilesFilter,
  type FilesPlace,
  type FilesSort,
} from "./filters";
import type { FolderRow } from "./rows";
import type { FolderTree, MaterializedRail, TreeNode } from "./fold";
import { binItemFromArtifact } from "../bin/rows";
import { planRestore, runRestore } from "../bin/restore";
import {
  artifactFingerprint,
  artifactFromRow,
  artifactName,
  deskEntryOf,
  fileStory,
  type ArtifactRow,
  type CompositionRow,
} from "./rows";
import { ACCOUNT_ANY, ACCOUNT_NONE, filterMenuPick, filterMenuValue, LABEL_ANY, otherFiltersOn, SOURCE_LABELS, sourcesOf } from "./filters";
import { accountIsArchived, accountName } from "../accounts/rows";
import { useAccountOptions } from "../accounts/tie";
import { Inspector } from "./Inspector";
import type { UploadTask, UploadTasksApi } from "./useUploadTasks";

// The browse (design D1, reshaped by epic memql#4842, then the "Places"
// redesign of Nov 2026): a rail of places and the Library's folder tree on the
// left, and on the right a Head (the picked file's icons, one search button,
// the one Add control) over a grid of folders and files. The search button
// opens a panel under the Head that holds search, kind, sort and Added (José,
// Nov 19 2026: "the point is that it's hidden").
//
// A CLICK ON A FILE ONLY SELECTS IT. The owner asked for no panel popping up on
// a click: double-click or Enter opens the file, and right-click holds its
// verbs, among them Details, which replaces the grid with the file's page (the
// Inspector -- labels, client, versions), so nothing that page offers is lost.

/** A desk folder shortcut, folded from shell state by the app root. */
export interface DeskFolderShortcut {
  folderId: string;
  name: string;
}

const PLACE_TITLE: Record<FilesPlace, string> = {
  library: "Library",
  desktop: "Desktop",
  bin: "Bin",
  materializer: "Materializer",
  shared: "Shared with me",
  sharing: "Sharing",
};

/** The rail's "Shared" folder, which the Head's trail names above its two places. */
const SHARED_FOLDER = "Shared";

export function BrowseSection({
  list,
  artifacts,
  counts,
  labelOptions,
  facetsError,
  files,
  foldersState,
  tree,
  content,
  archivedFolders,
  materialized,
  materializedRail,
  deskFolders,
  deskIndexByArtifactId,
  desksWithItems,
  filter,
  setFilter,
  selectedId,
  explicitSelection = null,
  onSelect,
  confirmBeforeArchive,
  askAbout,
  tasks,
  uploadFiles,
  uploadTree,
}: {
  list: LiveView<ArtifactRow> | null;
  artifacts: LibraryArtifactsFeed;
  counts: ReadonlyMap<string, number> | null;
  /** The labels in the place, for the kind menu's Label group. */
  labelOptions: readonly string[];
  facetsError: string;
  files: readonly Row[];
  foldersState: LiveState;
  tree: FolderTree;
  content: ArtifactRow[];
  /** Archived folders, flat and alphabetical -- the Bin place's folders. */
  archivedFolders: FolderRow[];
  /**
   * The composition behind each artifact that has one (epic memql#4981,
   * #4983) -- membership for the place, and the record's id for the handoff.
   */
  materialized: ReadonlyMap<string, CompositionRow>;
  /** The Materializer place's folders, and its honest total. */
  materializedRail: MaterializedRail;
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
  /** The owner-checked destination can open before the broad index finishes. */
  explicitSelection?: ArtifactRow | null;
  onSelect: (id: string) => void;
  confirmBeforeArchive: boolean;
  askAbout?: (tag: string) => void;
  tasks: UploadTask[];
  uploadFiles: UploadTasksApi["uploadFiles"];
  uploadTree: UploadTasksApi["uploadTree"];
}) {
  const { presence } = useMachinePresence();
  const { actions, registry } = useOs();
  const { config, access } = useSession();
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
  // The row actions' one refusal slot, carrying the SENTENCE with it. Three
  // verbs report here (archive, move, download) and a single
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
  const [vsNoAnswer, setVsNoAnswer] = useState(false);
  // The grid's selection: which file a click picked. It opens nothing; it only
  // marks the file a right-click, Enter or double-click is about.
  const [picked, setPicked] = useState("");
  // THE SHARE VIEW (Nov 20 2026): the file whose Share was pressed. It takes
  // the place of the tiles, the rail beside it, until its back arrow.
  const [shareTarget, setShareTarget] = useState<ArtifactRow | null>(null);
  const canShare = useCanShareFiles();

  // Every heavy verb the row menu offers is the inspector's verb, reached by
  // the SAME function -- the download decision is shared in actions/download,
  // the desk hand-off is one shell action. A right-click is a third entry point onto one set of actions,
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
      // The same one-line answer, for the same five seconds, as Save's.
      setRailNote(`Downloaded "${artifactName(row)}".`);
      setTimeout(() => setRailNote(""), 5000);
    } catch (err: unknown) {
      refuse("The download did not land.", "Nothing was saved.", err);
    }
  };

  const sendRowToDesk = (row: ArtifactRow) => {
    const outcome = actions.sendFileToDesk(deskEntryOf(row));
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

  const archiveOneRow = async (row: ArtifactRow) => {
    const query = connection?.query ?? null;
    if (query === null) {
      refuse("The file was not deleted.", "The file is where it was.", "Not connected to the cluster.");
      return;
    }
    setRowArchive(null);
    setRowNote(null);
    try {
      // Nothing is patched locally: the archive broadcasts, the row leaves
      // this list on the same feed, and it arrives in the Bin.
      await query.archiveArtifact({ artifactId: row.id });
    } catch (err: unknown) {
      refuse("The file was not deleted.", "The file is where it was.", err);
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
      refuse("The restore was refused.", "The file is still in the Bin.", "Not connected to the cluster.");
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
      refuse("The restore was refused.", "The file is still in the Bin.", err);
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
  const [folderCheck, setFolderCheck] = useState<{ id: string; rows: ArtifactRow[]; error: string } | null>(null);
  const checkingFolderId = folderMenu?.node.folder.id ?? pendingArchive;
  useEffect(() => {
    if (!checkingFolderId || !connection) { setFolderCheck(null); return; }
    const abort = new AbortController();
    setFolderCheck(null);
    void readArchiveMembers(connection.query, tree, checkingFolderId, abort.signal).then(rows => {
      if (!abort.signal.aborted) setFolderCheck({ id: checkingFolderId, rows, error: "" });
    }, error => {
      if (!abort.signal.aborted) setFolderCheck({ id: checkingFolderId, rows: [], error: String(error) });
    });
    return () => abort.abort();
  }, [connection, checkingFolderId, tree]);

  const runFolderArchive = async (folderId: string) => {
    const query = connection?.query ?? null;
    if (query === null) {
      setArchiveError("Not connected to the cluster, so nothing was deleted.");
      return;
    }
    setPendingArchive("");
    setArchiveError("");
    // The plan recomputes from LIVE rows at the moment of running -- which
    // is what makes an interrupted walk idempotent: whatever already landed
    // is simply absent from the next plan.
    try {
      const members = await readArchiveMembers(query, tree, folderId);
      const plan = planArchive(tree, members, folderId);
      setArchiveProgress({ done: 0, total: plan.artifactIds.length + plan.folderIds.length });
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
    filter.search.trim() !== "" || filter.place === "bin" || filter.place === "materializer" || filter.place === "shared" || filter.place === "sharing"
      ? ""
      : (filter.folderId ?? "");
  // NO ADD WHERE NOTHING CAN BE ADDED (Jesús, Nov 20 2026). The Bin holds
  // what you deleted, Shared with me holds other people's files, and Sharing
  // is the list of what you share: the "+" there only ever filed the new
  // thing at the Library root, out of sight, so it is absent (rule 12). A
  // file dropped on the window still lands in the Library root, as above.
  const canAdd = filter.place !== "bin" && filter.place !== "shared" && filter.place !== "sharing";

  const onDrop =(event: React.DragEvent) => {
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

  const rowMenuEntries = (row: ArtifactRow): MenuEntry[] => {
    // An explicit request, so it OPENS Ask (askAbout); absent where the host
    // has no Ask to open rather than an entry that does nothing.
    const ask = askAbout
      ? [
          {
            id: "ask",
            label: "Ask about this file",
            onSelect: () => askAbout(`app:files/browse file:${artifactName(row)}`),
          },
        ]
      : [];
    // THE ONE HANDOFF TO THE MATERIALIZER (epic memql#4981, #4983), and it is
    // ABSENT rather than disabled when it is not legal (DESIGN.md rule 12):
    // absent when this file was not composed, and absent when the caller
    // cannot open that app at all -- `openApp` no-ops on an app the registry
    // does not hold or the actor's rank does not admit, and a control that
    // silently does nothing is worse than one that is not there.
    const composition = materialized.get(row.id);
    const openInMaterializer =
      composition && canOpen(registry, MATERIALIZER_APP_ID)
        ? [
            {
              id: "materializer",
              label: "Open in Materializer",
              onSelect: () =>
                actions.openApp(MATERIALIZER_APP_ID, MATERIALIZER_MADE_SECTION, {
                  compositionId: composition.id,
                }),
            },
          ]
        : [];
    const download = {
      id: "download",
      label: "Download",
      onSelect: () => downloadRow(row),
    };
    // The file's page -- origin, client, labels, versions -- now that a click
    // no longer opens it. It replaces the grid, with the way back in the trail.
    const details = { id: "details", label: "Details", onSelect: () => onSelect(row.id) };
    if (row.archived) {
      return [
        {
          id: "restore",
          // The Bin's verb, kept verbatim (cohesion): one name for one action
          // wherever it appears.
          label: "Restore",
          disabled: connection === null,
          onSelect: () => restoreOneRow(row),
        },
        download,
        details,
        ...openInMaterializer,
        ...ask,
      ];
    }
    // The owner's order: Open and Download first, Delete last. Rename,
    // which he also asked for, is absent until the engine can do it (rule 12:
    // absent, never disabled). Share opens the share view in place of the
    // tiles (José, Nov 19: "another screen here in the middle"), only on a
    // file of the caller's own and only with the share part.
    const share = canShare && access !== null && row.ownerUserId === access.userId
      ? [{ id: "share", label: "Share", onSelect: () => { setRowMenu(null); setShareTarget(row); } }]
      : [];
    return [
      ...(!isZipArtifact(row) ? [{ id: "open", label: "Open", onSelect: () => openVsCodeFor(row) }] : []),
      download,
      ...share,
      details,
      { id: "desk", label: "Send to desktop", onSelect: () => sendRowToDesk(row) },
      {
        id: "move",
        label: "Move to folder",
        disabled: connection === null,
        onSelect: () => setRowMove(row),
      },
      ...openInMaterializer,
      ...ask,
      {
        id: "archive",
        // José's call (Nov 18 2026): the verb a person sees for this is "Delete",
        // everywhere files are concerned. A deleted file still goes to the Bin and
        // Restore puts it back; the confirm's second line says so, and an empty
        // folder, which is removed for good, says that instead. Code names keep
        // "archive" because that is the engine's mutation.
        label: "Delete",
        disabled: connection === null,
        onSelect: () =>
          confirmBeforeArchive ? setRowArchive(row) : archiveOneRow(row),
      },
    ];
  };

  const openVsCodeFor = (row: ArtifactRow) => {
    // A zip downloads; its promise goes back so the tile holds a burst.
    if (isZipArtifact(row)) return downloadRow(row);
    setVsNoAnswer(false);
    openInVsCode(config.domain, row.id, () => setVsNoAnswer(true), undefined, editorFilename(row));
  };

  // A file shared WITH the caller offers exactly two acts: take a copy home,
  // or keep one of your own. It is somebody else's file, so nothing here
  // opens, moves, archives or re-shares it.
  const downloadNotice = async (notice: SharedNotice) => {
    setRowNote(null);
    try {
      await downloadArtifact({
        artifactId: notice.artifactId,
        name: notice.title,
        // The sharer's file row is not the recipient's to read; the notice
        // carries the size the download plan needs, so it answers the read.
        fileId: notice.artifactId,
        readFile: async () => ({ sizeBytes: notice.size, name: notice.title }),
        bearer: () => authSource.bearer(),
      });
      // The same one-line answer, for the same five seconds, as Save's.
      setRailNote(`Downloaded "${notice.title}".`);
      setTimeout(() => setRailNote(""), 5000);
    } catch (err: unknown) {
      refuse("The download did not land.", "Nothing was saved.", err);
    }
  };

  const saveNotice = async (notice: SharedNotice) => {
    const query = connection?.query ?? null;
    if (query === null) {
      refuse("The copy was not saved.", "Nothing was added to your Library.", "Not connected to the cluster.");
      return;
    }
    setRowNote(null);
    try {
      await query.librarySaveSharedCopy({ artifactId: notice.artifactId });
      setRailNote(`Saved "${notice.title}" to your Library.`);
      setTimeout(() => setRailNote(""), 5000);
    } catch (err: unknown) {
      refuse("The copy was not saved.", "Nothing was added to your Library.", err);
    }
  };

  const noticeMenuEntries = (notice: SharedNotice): MenuEntry[] => [
    // The acts hand back their promise: drawn as the Head's icons, each takes
    // no second press until it has finished (kit/oneAtATime; José, Nov 19:
    // twenty clicks on Save made eighteen copies).
    { id: "download", label: "Download", onSelect: () => downloadNotice(notice) },
    { id: "save", label: "Save to my Library", disabled: connection === null, onSelect: () => saveNotice(notice) },
  ];
  const [noticeMenu, setNoticeMenu] = useState<{ x: number; y: number; notice: SharedNotice } | null>(null);

  // The Sharing place's Stop. The share row leaves on its own event, so
  // nothing is patched here.
  const stopShare = async (shareId: string) => {
    const query = connection?.query ?? null;
    if (query === null) throw new Error("Not connected to the cluster, so the share was not stopped.");
    await query.libraryShareStop({ shareId });
  };

  const selected = content.find((r) => r.id === selectedId)
    ?? (explicitSelection?.id === selectedId ? explicitSelection : null);
  // The picked file, for the Head's icons. Absent once it leaves the list --
  // deleted, moved away, or filtered out -- so the icons never act on a file
  // nobody can see.
  const pickedRow = picked === "" ? null : list?.snapshot.rows.find((r) => r.id === picked) ?? null;

  // SHARING (the engine's library sharing, Nov 2026). Share is a verb on the
  // caller's own live files (rowMenuEntries) and opens the share view.
  // Shared with me is a place of its own, fed by the caller's share notices
  // rather than their Library.
  const shared = filter.place === "shared";
  const notices = useSharedWithMe(shared);
  // The search panel asks Shared with me the same questions, over its notices.
  const noticeList = useLiveView<Row, SharedNotice>(notices.source, `files:shared:${filter.search}|${filter.type}|${filter.added}|${filter.sort}`, (rows) => {
    const now = Date.now();
    const needle = filter.search.trim().toLowerCase();
    const byId = new Map(rows.map(noticeFromRow).filter((n) => n.id !== "" && n.title.toLowerCase().includes(needle)).map((n) => [n.id, n]));
    return [...byId.values()].map(noticeTile)
      .filter((tile) => (filter.type === "all" || fileKindOf(tile) === filter.type) && addedWithin(tile.createdAt, filter.added, now))
      .sort(compareRows(filter.sort, (tile) => byId.get(tile.id)?.size))
      .flatMap((tile) => byId.get(tile.id) ?? []);
  });
  const pickedNotice = shared && picked !== "" ? noticeList?.snapshot.rows.find((n) => n.id === picked) ?? null : null;
  // SHARING: the caller's own shares, across every file (Nov 19 2026). Its
  // rows are shares, not files, so nothing here is picked; each row carries
  // its one act, Stop. A share that runs out while the page is open leaves at
  // the next tick of the list's clock (SharingPlace), with no event behind it.
  const sharingPlace = filter.place === "sharing";
  const outgoing = useSharing(sharingPlace);
  // The search panel asks Sharing the same questions it asks Shared with
  // me: the file's name (or who has it), its kind, when it was shared.
  const outgoingList = useLiveView<Row, OutgoingShare>(outgoing.source, `files:sharing:${filter.search}|${filter.type}|${filter.added}|${filter.sort}`,
    (rows) => askOfShares(rows.map(outgoingFromRow).filter((share) => share.id !== "" && shareStands(share)), filter));
  const shareTitles = useShareTitles(sharingPlace ? outgoingList?.snapshot.rows ?? [] : []);
  // SHARED BY (item 13; Jesús's pick, Nov 20, draft B): someone at member rank
  // or above may look past their own shares -- "Me", "Everyone", or one
  // person -- and Stop what the engine lets them. It starts on "Me", which is
  // the live list above; the rest is librarySharesOverseen, which the engine
  // already narrows to the shares this person may stop. The rule (Jesús, Nov 20
  // checklist item 14): your own plus everyone ranked strictly below you;
  // owners and admins never see their equals, developers do see each other,
  // members see viewers', viewers only their own -- so the floor here is the
  // member rung, the lowest rank with anyone below it. The drop-down is only
  // the way in; the engine decides, on the list and on every Stop.
  const overseer = sharingPlace && roleAdmits(access?.role ?? "", { min: "user" });
  const [sharedBy, setSharedBy] = useState("me");
  const overseen = useSharesOverseen(overseer);
  const overseenList = useLiveView<Row, OutgoingShare>(overseen.source, `files:overseen:${sharedBy}:${filter.search}|${filter.type}|${filter.added}|${filter.sort}`,
    (rows) => askOfShares(rows.map(outgoingFromRow).filter((share) => share.id !== "" && shareStands(share) &&
      (sharedBy === "everyone" || share.ownerUserId === sharedBy)), filter));
  const sharers = overseenSharers(overseen.snapshot.rows);
  const byOthers = overseer && sharedBy !== "me";
  const pickedActs = selected !== null ? null
    : pickedNotice !== null ? { name: pickedNotice.title, entries: noticeMenuEntries(pickedNotice) }
    : pickedRow !== null ? { name: artifactName(pickedRow), entries: rowMenuEntries(pickedRow) }
    : null;
  const searching = filter.search.trim() !== "";

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
  const asked = searching || filter.type !== "all" || filter.added !== "any";
  const narrowed = asked || (!shared && !sharingPlace && (
    filter.kind !== "all" ||
    filter.source !== "all" ||
    filter.accountId !== ACCOUNT_ANY ||
    filter.label !== LABEL_ANY ||
    filter.folderId !== ""));
  // Folders is a question only folders answer: the file list steps aside.
  const foldersOnly = filter.type === "folders";
  // While the whole place is still being read for an order or filter the
  // pages cannot answer (useLibrary.ts), an empty list is not yet an answer.
  const draining = !shared && !sharingPlace && !foldersOnly && needsWholePlace(filter) && artifacts.hasMore;
  const emptyText = narrowed
    ? "Nothing matches. Clear the search or filters to see your files."
    : shared
    ? "Nothing has been shared with you."
    : sharingPlace
    ? "You are not sharing any files."
    : filter.place === "desktop"
      ? "Nothing on your desks. Send a file to the desk from the Library, or drop one onto the desk."
      : filter.place === "bin"
        ? "The Bin is empty. A file you delete from the Library waits here until you put it back."
        : filter.place === "materializer"
          ? "Nothing has been materialized yet. The Materializer composes a file -- a document, a spreadsheet, a PDF -- out of what is in the memory graph."
          : "Nothing in your Library yet. Drop a file onto the desk or upload one here.";

  // The Head names the scope ONCE (DESIGN.md rule 7): the place, or the
  // folder being looked at inside it.
  const headTitle =
    !searching && filter.folderId !== "" && filter.folderId !== null
      ? folderNameOf(filter.folderId)
      : PLACE_TITLE[filter.place];

  // WHERE THE FOLDER SITS, for the window's trail row. The Head named only the
  // open folder, so three levels down the trail read "Invoices" -- the same
  // word it would read for a root folder of that name -- and the way back up
  // was the rail alone. Folders nest (`parentFolderId`), so the depth is real
  // and the trail says it: the place, every ancestor, then this folder. Each
  // ancestor opens itself; the place clears the folder.
  //
  // The walk is BOUNDED and remembers what it has seen. `parentFolderId` is
  // data, and a cycle in it must cost a short trail rather than a hung page.
  const headTrail = ((): Breadcrumb[] | undefined => {
    // Shared with me and Sharing sit inside the rail's "Shared" folder, and the
    // trail says so. "Shared" is a folder with no page of its own, so it is a
    // name here, not a way back.
    if (shared || sharingPlace) return [{ label: SHARED_FOLDER }, { label: PLACE_TITLE[filter.place] }];
    if (searching || filter.folderId === "" || filter.folderId === null) return undefined;
    const chain: string[] = [];
    const seen = new Set<string>();
    for (let id: string = filter.folderId; id !== "" && !seen.has(id) && chain.length < 32;) {
      seen.add(id);
      chain.unshift(id);
      id = tree.byId.get(id)?.folder.parentFolderId ?? "";
    }
    return [
      { label: PLACE_TITLE[filter.place], onSelect: () => patch({ folderId: "" }) },
      ...chain.slice(0, -1).map((folderId) => ({ label: folderNameOf(folderId), onSelect: () => patch({ folderId }) })),
      { label: folderNameOf(filter.folderId) },
    ];
  })();

  const placeFolders = filter.place === "library" ? tree.roots.map(node => node.folder)
    : filter.place === "desktop" ? deskFolders.flatMap(shortcut => { const node = tree.byId.get(shortcut.folderId); return node ? [node.folder] : []; })
    : filter.place === "bin" ? archivedFolders
    : shared || sharingPlace ? []
    : materializedRail.folders.map(entry => entry.folder);
  // A search looks at the whole place, as it does for files: every folder in
  // the Library's tree, or the place's own folders elsewhere, by name.
  const needle = filter.search.trim().toLowerCase();
  const browsedFolders = searching
    ? (filter.place === "library" ? [...tree.byId.values()].map(node => node.folder).sort((a, b) => a.name.localeCompare(b.name)) : placeFolders)
      .filter(folder => folder.name.toLowerCase().includes(needle))
    : filter.folderId ? (tree.byId.get(filter.folderId)?.children.map(node => node.folder) ?? [])
    : placeFolders;
  // A kind of file hides the folders; Added asks of them what it asks of files.
  const now = Date.now();
  const visibleFolders = filter.type !== "all" && !foldersOnly ? []
    : browsedFolders.filter(folder => addedWithin(folder.createdAt, filter.added, now));
  // The Head's meta is what the LIST is showing right now -- scoped folder,
  // facets and search included -- never a different population's number
  // beside the scope's name. Showing Folders, that is the folders.
  const listedSnapshot = shared ? noticeList?.snapshot : sharingPlace ? (byOthers ? overseenList : outgoingList)?.snapshot : list?.snapshot;
  const listedCount = foldersOnly ? visibleFolders.length : listedSnapshot?.rows.length ?? 0;
  const pageRoot = usePageScroll(`${filter.place}:${filter.folderId ?? ""}:${selectedId}`);

  const clearFilters = () =>
    patch({
      kind: "all",
      source: "all",
      accountId: ACCOUNT_ANY,
      label: LABEL_ANY,
      search: "",
      type: "all",
      added: "any",
      folderId: "",
    });

  // THE HIDDEN SEARCH PANEL (José, Nov 19 2026; Jesús picked two lines, with
  // Fleet's search bar reused as it is). One button on the Head opens it under
  // the Head, pushing the tiles down rather than covering them, and closes it.
  // CLOSING ENDS THE SEARCH: a kind or Added window left on while the panel is
  // hidden would narrow the files with nothing on screen saying so. The order
  // stays, since it hides nothing.
  const [searchOpen, setSearchOpen] = useState(false);
  const searchInput = useRef<HTMLInputElement | null>(null);
  const mainRef = useRef<HTMLDivElement | null>(null);
  const searchPanelId = useId();
  useEffect(() => { if (searchOpen) searchInput.current?.focus(); }, [searchOpen]);
  // Read only while the panel is open: the Account group is the one reader.
  const accountOptions = useAccountOptions(searchOpen);
  // THE KIND MENU (Jesús, Nov 20 2026, draft p17 Option C): the kinds of
  // file, then Label, Source and Account as more groups, each its own single
  // choice with its own check (filters.ts `filterMenuPick`). The button reads the
  // kind, plus "+N" while N of the other three are on: the drawing shows only
  // the kind, and this is the least that still says something else narrows
  // the files. Shared with me lists other people's notices, which carry no
  // label, source or client, so it keeps the kinds alone (as Refine was
  // absent there).
  const otherOn = shared ? 0 : otherFiltersOn(filter);
  const kindName = FILE_TYPES.find(([type]) => type === filter.type)?.[1] ?? filter.type;
  const option = (group: Parameters<typeof filterMenuValue>[0], value: string, name: string, checked: boolean) =>
    <option key={value} value={filterMenuValue(group, value)} data-checked={checked}>{name}</option>;
  // Only the sources some loaded file has (filters.ts `sourcesOf`).
  const shownSources = sourcesOf(content, filter.source);
  const filterMenu = <>
    <optgroup label="Kind">{FILE_TYPES.map(([type, name]) => option("type", type, name, filter.type === type))}</optgroup>
    {shared ? null : <>
      {/* ABSENT WHEN NOTHING IS LABELLED (the old Refine's rule): a group
          offering only "Any label" is a question with no answers. */}
      {labelOptions.length === 0 && filter.label === LABEL_ANY ? null : <optgroup label="Label">
        {option("label", LABEL_ANY, "Any label", filter.label === LABEL_ANY)}
        {labelOptions.map((label) => option("label", label, label, filter.label === label))}
      </optgroup>}
      <optgroup label="Source">
        {option("source", "all", SOURCE_LABELS.all, filter.source === "all")}
        {shownSources.map((source) => option("source", source, SOURCE_LABELS[source], filter.source === source))}
      </optgroup>
      {/* "No account" is its own answer: what still needs filing (epic memql#4800).
          Called Account, not Client (Jesús, Nov 20 2026): it is the Accounts app's list. */}
      <optgroup label="Account">
        {option("client", ACCOUNT_ANY, "Any account", filter.accountId === ACCOUNT_ANY)}
        {option("client", ACCOUNT_NONE, "No account", filter.accountId === ACCOUNT_NONE)}
        {accountOptions.map((account) => option("client", account.id,
          accountIsArchived(account) ? `${accountName(account)} (archived)` : accountName(account), filter.accountId === account.id))}
      </optgroup>
    </>}
  </>;
  // Label, source and client are in the kind menu, so they end with it too.
  const toggleSearch = () => {
    if (searchOpen) patch({ search: "", type: "all", added: "any", label: LABEL_ANY, source: "all", accountId: ACCOUNT_ANY });
    setSearchOpen(!searchOpen);
  };

  return (
    <div
      ref={pageRoot}
      className="os-files"
      data-detail={selected !== null || undefined}
      onDragOver={(event) => {
        if (!event.dataTransfer.types.includes("Files")) return;
        event.preventDefault();
        event.stopPropagation();
      }}
      onDrop={onDrop}
    >
      <div hidden={selected !== null} className="os-files-browse">
      <FilesRail
        tree={tree}
        place={filter.place}
        folderId={filter.folderId ?? ""}
        onPlace={(place) => { onSelect(""); setPicked(""); setShareTarget(null); setSharedBy("me"); patch({ place, folderId: "", search: "" }); }}
        onFolder={(folderId) => { onSelect(""); setPicked(""); setShareTarget(null); patch({ place: "library", folderId, search: "" }); }}
        onFolderMenu={(x, y, node) => setFolderMenu({ x, y, node })}
      />
      {shareTarget !== null ? (
        <div className="os-files-main">
          <ShareView key={shareTarget.id} file={shareTarget} backLabel={headTitle} onBack={() => setShareTarget(null)} />
        </div>
      ) : null}
      {/* Hidden, not unmounted, while the share view stands in its place, so
          the way back finds the files where they were. */}
      <div className="os-files-main" ref={mainRef} hidden={shareTarget !== null}>
      <Head navigationKey={`browse:${filter.place}:${filter.folderId ?? ""}`} title={headTitle} breadcrumbs={headTrail} navigation={selected === null && shareTarget === null} back={filter.folderId ? {label: PLACE_TITLE[filter.place], onSelect: () => patch({folderId: tree.byId.get(filter.folderId!)?.folder.parentFolderId ?? ""})} : undefined} meta={listCount(listedSnapshot, listedCount) === undefined ? undefined : `${listedCount}${!shared && !sharingPlace && artifacts.hasMore ? "+" : ""}`}>
        {/* ONE ROW, THREE AREAS (José, Nov 19 2026; Jesús picked Draft A): the
            picked file's icons beside the title, one search button on the
            right, and Add apart at the far right.
            ONE WAY TO ADD SOMETHING (DESIGN.md rule 1: at most one primary
            action). Uploading a file and making a folder are the same
            question -- "put something here" -- and they were answered in two
            places: a primary button up here and a rail ACTION wedged between
            the Library tree and the Desktop place, where it read as a folder
            you could open. The rail action is gone; the answers live on this
            button, and each lands in the folder currently being looked at. */}
        {/* THE PICKED FILE'S ACTS, AS ICONS (owner, Nov 18 2026), the way a
            deployable's page carries its own. The same entries as the file's
            right-click menu, so the two can never disagree; an entry the menu
            would show disabled is absent here (rule 12). A hairline keeps
            them apart from the title. */}
        {pickedActs !== null ? (
          <span className="os-files-picked-acts" role="group" aria-label={`Selected: ${pickedActs.name}`}>
            {pickedActs.entries.filter((entry) => !entry.disabled).map((entry) => (
              // Named at once and above the icon, out of the pointer's way: the
              // browser's own tooltip waited about two seconds (Jesús, Nov 20 2026).
              <IconButton key={entry.id} label={entry.label} tip="above" onClick={entry.onSelect}>
                {PICKED_ICON[entry.id]}
              </IconButton>
            ))}
          </span>
        ) : null}
        {overseer ? (
          <span className="os-files-sharedby">
            <span aria-hidden="true">Shared by</span>
            <Select id="files-shared-by" label="Shared by" value={sharedBy} onChange={setSharedBy}>
              <option value="me">Me</option>
              <option value="everyone">Everyone</option>
              {sharers.map(([id, name]) => <option key={id} value={id}>{name}</option>)}
            </Select>
          </span>
        ) : null}
        <IconButton
          label="Search"
          className="os-files-search-toggle"
          aria-expanded={searchOpen}
          aria-controls={searchOpen ? searchPanelId : undefined}
          onClick={toggleSearch}
        >
          <Search size={16} aria-hidden />
        </IconButton>
        {canAdd ? <AddMenu
          disabled={connection === null}
          onUpload={() => pickRef.current?.click()}
          newFileKinds={canOpen(registry, MATERIALIZER_APP_ID) ? [{
            label: "Markdown file",
            run: () => actions.openApp(
              MATERIALIZER_APP_ID, MATERIALIZER_COMPOSE_SECTION, composeIntent({
                format: "markdown",
                ...(destinationFolderId ? { folderId: destinationFolderId } : {}),
                returnTo: { appId: "files", sectionId: "browse" },
              }),
            ),
          }] : []}
          onNewFolder={() => void newFolderIn(destinationFolderId)}
        /> : null}
      </Head>
      {searchOpen ? (
        <div id={searchPanelId} className="os-files-search">
          <div className="os-files-search-lines">
            <CardCollectionSearch
              label="Search files"
              query={filter.search}
              onQueryChange={(search) => patch({ search })}
              inputRef={searchInput}
              inputProps={{ "aria-label": "Search" }}
              clearLabel="Clear the search"
              mode={{
                label: "Filter files",
                value: filterMenuValue("type", filter.type),
                display: otherOn > 0 ? `${kindName} +${otherOn}` : kindName,
                children: filterMenu,
                onChange: (picked) => patch(filterMenuPick(picked)),
              }}
            />
            <div className="os-files-search-order">
              <Select id={`${searchPanelId}-sort`} label="Sort" icon={<ArrowUpDown size={13} aria-hidden />} value={filter.sort} onChange={(sort) => patch({ sort: sort as FilesSort })}>
                {FILES_SORTS.map(([value, name]) => <option key={value} value={value}>{name}</option>)}
              </Select>
              <Select id={`${searchPanelId}-added`} label="Added" value={filter.added} onChange={(added) => patch({ added: added as AddedWithin })}>
                {ADDED_WITHIN.map(([value, name]) => <option key={value} value={value}>{name}</option>)}
              </Select>
            </div>
          </div>
        </div>
      ) : null}
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

      {railNote ? <Caption>{railNote}</Caption> : null}
      <div className="os-files-body">
        <FileFolders folders={visibleFolders} counts={searching ? null : counts} loading={foldersState === "seeding"}
          renamingId={renamingFolderId} onOpen={folderId => patch({ folderId, search: "" })}
          onRename={(folderId, name) => void renameFolder(folderId, name)} onCancelRename={() => setRenamingFolderId("")}
          onMenu={(x, y, folder) => folder.archived ? setArchivedFolderMenu({x,y,folder}) : setFolderMenu({x,y,node:tree.byId.get(folder.id)!})} />

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
          {/* THE CONFIRM SAYS WHAT HAPPENS TO THE FOLDER. The act is "Delete"
              either way (José, Nov 18 2026), so the second line carries the
              difference: a folder holding files goes to the Bin, where Restore
              puts it back; a folder holding no file at any depth is removed
              for good -- the one act in this app Restore cannot undo. */}
          {pendingArchive !== "" ? (
            (() => {
              if (!folderCheck || folderCheck.id !== pendingArchive) return <Caption>Checking folder contents…</Caption>;
              if (folderCheck.error) return <Notice tone="error" sentence="The folder could not be checked." detail={folderCheck.error}>
                <Button onClick={() => setPendingArchive("")}>Cancel</Button>
              </Notice>;
              const plan = planArchive(tree, folderCheck.rows, pendingArchive);
              const name = folderNameOf(pendingArchive);
              const keeping = subtreeHoldsArtifact(tree, folderCheck.rows, pendingArchive);
              const emptied = plan.deleteFolderIds.length;
              // THE WHOLE NOTICE IS THE ASK, question and all: another folder
              // chosen while this one is open changes the question under the
              // same buttons (see InPlaceAsk).
              return (
                <InPlaceAsk>
                  <Notice
                    tone="warn"
                    sentence={
                      keeping
                        ? `Delete "${name}" and its ${plan.itemCount} items?`
                        : `Delete "${name}"?`
                    }
                    next={
                      keeping
                        ? `Everything inside goes to the Bin too, children first, and Restore puts it back.${
                            emptied > 0
                              ? ` The ${emptied} empty ${emptied === 1 ? "folder" : "folders"} inside are removed for good -- there is nothing in them to put back.`
                              : ""
                          }`
                        : `It holds no files at any depth${
                            emptied > 1 ? `, and nor do the ${emptied - 1} folders inside it` : ""
                          }. There is nothing to put back, so it is removed for good rather than kept in the Bin.`
                    }
                  >
                    <div className="os-files-confirm">
                      <Button tone="danger" busyLabel="Deleting…" onClick={() => runFolderArchive(pendingArchive)}>
                        Delete
                      </Button>
                      <Button onClick={() => setPendingArchive("")}>Cancel</Button>
                    </div>
                  </Notice>
                </InPlaceAsk>
              );
            })()
          ) : null}
          {archiveProgress !== null ? (
            <Caption>
              Deleting -- {archiveProgress.done} of {archiveProgress.total}. Interrupting is safe:
              running it again deletes only the remainder.
            </Caption>
          ) : null}
          {archiveError !== "" ? (
            <Notice tone="error" sentence="Deleting stopped." detail={archiveError}>
              <Button onClick={() => setArchiveError("")}>Dismiss</Button>
            </Notice>
          ) : null}
          {tasks.map((task) => (
            <UploadPlaceholder key={task.id} task={task} />
          ))}
          {sharingPlace && !foldersOnly ? (
            <>
              {byOthers ? <AutoRefresh onRefresh={() => overseen.reseed({ ifIdle: true })} /> : null}
              <SharingPlace
                key={byOthers ? "others" : "mine"}
                source={byOthers ? overseenList : outgoingList}
                label={headTitle}
                emptyText={byOthers ? (sharedBy === "everyone" ? "Nobody is sharing a file you can stop." : "They are not sharing a file you can stop.") : emptyText}
                showSharer={byOthers}
                titleOf={(share) => shareTitles.get(share.artifactId) ?? content.find((row) => row.id === share.artifactId)?.title ?? "Untitled file"}
                onStop={async (share) => {
                  setRowNote(null);
                  try {
                    await stopShare(share.id);
                  } catch (err: unknown) {
                    refuse("Sharing did not stop.", "The file is still shared.", err);
                  }
                  // Somebody else's share sends no event here: read again.
                  if (overseer) overseen.reseed();
                }}
              />
            </>
          ) : (
          <div className="os-files-files">
          {foldersOnly ? (
            visibleFolders.length === 0 && foldersState !== "seeding" ? <Caption>{emptyText}</Caption> : null
          ) : shared ? (
            <LiveList<SharedNotice>
              key={`shared|${filter.search}|${filter.type}|${filter.added}|${filter.sort}`}
              source={noticeList}
              rowId={(n) => n.id}
              // A renewal moves expiresAt: the file is shared with you for longer.
              fingerprint={(n) => `${n.title}|${n.expiresAt}`}
              label={headTitle}
              emptyText={emptyText}
              renderRow={(notice, tick) => (
                <FileTile
                  row={noticeTile(notice)}
                  caption={`from ${notice.sharerDisplayName || "someone"}`}
                  fixed
                  picked={picked === notice.id}
                  dim={false}
                  hint={`Shared with you until ${new Date(notice.expiresAt).toLocaleDateString(undefined, { month: "short", day: "numeric" })}`}
                  tick={tick}
                  onPick={() => setPicked(notice.id)}
                  onOpen={() => downloadNotice(notice)}
                  onMenu={(x, y) => setNoticeMenu({ x, y, notice })}
                />
              )}
            />
          ) : (
          <LiveList<ArtifactRow>
            key={`${filter.place}|${filter.folderId ?? "~"}|${filter.kind}|${filter.source}|${filter.accountId}|${filter.label}|${filter.search}|${filter.type}|${filter.added}|${filter.sort}`}
            source={list}
            rowId={(r) => r.id}
            fingerprint={artifactFingerprint}
            label={headTitle}
            emptyText={draining ? undefined : emptyText}
            renderRow={(row, tick) => {
              // What the list row used to print beside the name -- where the
              // file came from, its folder while searching, its desk -- now
              // rides the tile's tooltip, so the grid shows names alone.
              const story = fileStory(row, row.producedByWorkerId ? presence(row.producedByWorkerId) : null, materialized.get(row.id) ?? null);
              const deskIndex = filter.place === "desktop" && desksWithItems > 1 ? (deskIndexByArtifactId.get(row.id) ?? null) : null;
              const hint = [
                story.sentence,
                (searching || !filter.folderId) && row.folderId !== "" ? `In ${folderNameOf(row.folderId)}` : "",
                deskIndex !== null ? `On desk ${deskIndex + 1}` : "",
                row.kind === "document" && row.validationStatus !== "" ? `Validation: ${row.validationStatus}` : "",
              ].filter(Boolean).join(" · ");
              return (
                <FileTile
                  row={row}
                  picked={picked === row.id}
                  dim={row.archived}
                  hint={hint}
                  tick={tick}
                  onPick={() => setPicked(row.id)}
                  // An archived file has nothing to open in an editor; its page
                  // is where Restore and its history are.
                  onOpen={() => (row.archived ? onSelect(row.id) : openVsCodeFor(row))}
                  onMenu={(x, y) => setRowMenu({ x, y, row })}
                />
              );
            }}
          />
          )}
          </div>
          )}
          {shared || sharingPlace || foldersOnly ? null : <LoadMore feed={artifacts} />}
          {facetsError ? <Caption>Folder counts could not be updated. {facetsError}</Caption> : null}
          {/* THE EMPTY STATE OFFERS THE WAY OUT (DESIGN.md rule 4's shape:
              an empty list that does not explain itself is the failure).
              `emptyText` says a filter is why; this is the act that undoes
              it, and it exists only while there IS something to undo -- a
              standing Clear control over a full list would be exactly the
              furniture rule 2 removes. */}
          {narrowed && listedCount === 0 && tasks.length === 0 && !draining ? (
            <div className="os-files-clear">
              <Button onClick={clearFilters}>Clear the search and filters</Button>
            </div>
          ) : null}
        </div>
      </div>
      </div>

      </div>
        {selected === null ? null : (
          <Inspector
            key={selected.id}
            row={selected}
            files={files}
            composition={materialized.get(selected.id) ?? null}
            folderNameOf={folderNameOf}
            presence={presence}
            backLabel={headTitle}
            breadcrumbs={[...(headTrail ?? [{label: headTitle}]).map((crumb, i, all) => i === all.length - 1 ? {...crumb, onSelect: () => onSelect("")} : {...crumb, onSelect: () => { onSelect(""); crumb.onSelect?.(); }}), {label: artifactName(selected)}]}
            onClose={() => onSelect("")}
          />
        )}

      {/* THE ROW'S MENU IS THE INSPECTOR'S ACTION SET (memql#4860 wave).
          It used to hold one entry -- Move to Bin -- which made a right-click
          look like it had failed to load rather than like the surface it is.
          Every verb here reaches the same function the inspector's button
          does; downloads run the shared implementation. */}
      {rowMenu !== null ? (
        <ContextMenu
          x={rowMenu.x}
          y={rowMenu.y}
          label="File"
          entries={rowMenuEntries(rowMenu.row)}
          onClose={() => setRowMenu(null)}
        />
      ) : null}
      {noticeMenu !== null ? (
        <ContextMenu
          x={noticeMenu.x}
          y={noticeMenu.y}
          label="Shared file"
          entries={noticeMenuEntries(noticeMenu.notice)}
          onClose={() => setNoticeMenu(null)}
        />
      ) : null}
      {archivedFolderMenu !== null ? (
        <ContextMenu
          x={archivedFolderMenu.x}
          y={archivedFolderMenu.y}
          label="Deleted folder"
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
        // THE WHOLE NOTICE IS THE ASK, question and all: another file chosen
        // while this one is open changes the question under the same buttons
        // (see InPlaceAsk).
        <InPlaceAsk>
          <Notice
            tone="warn"
            sentence={`Delete "${artifactName(rowArchive)}"?`}
            next="It goes to the Bin with its bytes, its history and everywhere it came from, and the Bin can put it back."
          >
            <div className="os-files-confirm">
              <Button tone="danger" busyLabel="Deleting…" onClick={() => archiveOneRow(rowArchive)}>
                Delete
              </Button>
              <Button onClick={() => setRowArchive(null)}>Cancel</Button>
            </div>
          </Notice>
        </InPlaceAsk>
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
              {/* The rail's own tree, as the picker's rows: every folder the tree placed is a live one. */}
              <FolderSelect
                id={`files-move-${rowMove.id}`}
                label="Move to folder"
                value={rowMove.folderId}
                onChange={(folderId) => void moveRowTo(rowMove, folderId)}
                options={folderOptions([...tree.byId.values()].map((node) => node.folder))}
              />
            </span>
            <Button onClick={() => setRowMove(null)}>Cancel</Button>
          </div>
        </Notice>
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
              // José's call (Nov 18 2026): the verb a person sees for this is "Delete",
              // everywhere files are concerned. A deleted file still goes to the Bin and
              // Restore puts it back; the confirm's second line says so, and an empty
              // folder, which is removed for good, says that instead. Code names keep
              // "archive" because that is the engine's mutation.
              label: !folderCheck || folderCheck.id !== folderMenu.node.folder.id ? "Checking contents…"
                : folderCheck.error ? "Folder check failed"
                : "Delete",
              disabled: connection === null || !folderCheck || folderCheck.id !== folderMenu.node.folder.id || !!folderCheck.error,
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

/** One kind of file "New file" can make. Add an entry to offer another kind. */
type NewFileKind = { label: string; run: () => void };

/**
 * The Add menu: one primary control, three ways to put something in the
 * place being looked at, in Jesús's order (Nov 20 2026): Upload files, New
 * folder, New file. "New file" is not an action of its own: it opens a side
 * menu of the kinds of file that can be made (today only Markdown), so more
 * kinds are one more entry in `newFileKinds`.
 *
 * NO HEADING. The menu once named the destination ("Into Library"); Jesús
 * removed it ("Remove it. I don't like it."). Everything still lands in the
 * folder being looked at (the Library root while searching or in Archive).
 */
function AddMenu({
  disabled,
  onUpload,
  newFileKinds,
  onNewFolder,
}: {
  disabled: boolean;
  onUpload: () => void;
  newFileKinds: NewFileKind[];
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
      {/* A bare "+" in the accent's outline, no word (José: "make it more
          noticeable"; Jesús's pick, Nov 20 2026). Its tooltip says Add. */}
      <AddButton label="Add" outlined aria-expanded={open} onClick={() => setOpen(!open)} />
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
          {newFileKinds.length > 0 ? (
            <Submenu label="New file" icon={<FilePlus size={13} aria-hidden />}>
              {newFileKinds.map((kind) => (
                <button key={kind.label} type="button" role="menuitem" className="os-menu-item" onClick={() => choose(kind.run)}>
                  <FileText size={13} aria-hidden /> {kind.label}
                </button>
              ))}
            </Submenu>
          ) : null}
        </div>
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

// One icon per row-menu entry id. An id with no icon here renders an empty
// button, which the test over every entry catches.
const PICKED_ICON: Record<string, ReactNode> = {
  open: <SquareArrowOutUpRight size={16} aria-hidden />,
  download: <Download size={16} aria-hidden />,
  details: <FileText size={16} aria-hidden />,
  share: <Share2 size={16} aria-hidden />,
  desk: <Monitor size={16} aria-hidden />,
  move: <FolderInput size={16} aria-hidden />,
  materializer: <Layers size={16} aria-hidden />,
  ask: <Mark size={16} aria-hidden />,
  archive: <Trash2 size={16} aria-hidden />,
  restore: <RotateCcw size={16} aria-hidden />,
  save: <Import size={16} aria-hidden />,
};

/** The search panel's questions, asked of the caller's shares (Sharing). */
function askOfShares(shares: OutgoingShare[], filter: FilesFilter): OutgoingShare[] {
  const now = Date.now();
  const needle = filter.search.trim().toLowerCase();
  const byId = new Map(shares
    .filter((share) => !needle || share.title.toLowerCase().includes(needle) || share.recipientDisplayName.toLowerCase().includes(needle))
    .map((share) => [share.id, share]));
  return [...byId.values()].map((share) => artifactFromRow({ id: share.id, kind: "file", title: share.title, createdAt: share.createdAt }))
    .filter((tile) => (filter.type === "all" || fileKindOf(tile) === filter.type) && addedWithin(tile.createdAt, filter.added, now))
    .sort(compareRows(filter.sort))
    .flatMap((tile) => byId.get(tile.id) ?? []);
}

/** What a file tile needs of a shared file: its name, its type and when it was shared. */
function noticeTile(notice: SharedNotice): ArtifactRow {
  return artifactFromRow({ id: notice.id, kind: "file", title: notice.title, format: notice.format, mimeType: notice.mimeType, createdAt: notice.createdAt });
}
