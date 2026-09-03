import { useCallback, useEffect, useMemo, useState } from "react";
import { Concepts, newShortId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useAuthSource } from "../../auth/context";
import { EdgeUploadProvider } from "../../items/edgeUpload";
import type { UploadProvider } from "../../items/upload";
import { Check, Head, Panel } from "../../kit";
import { useOsConnection } from "../../live/connection";
import { useLiveView } from "../../live/liveView";
import { useOs } from "../../chrome/state";
import { AppLogsSection } from "../../logs/AppLogsSection";
import type { OsAppProps } from "../../system/registry";
import { BackupsSection } from "./backups/BackupsSection";
import { backupFromRow, type BackupRow } from "./backups/rows";
import { useBackupsFeed } from "./backups/useBackups";
import { BrowseSection, type DeskFolderShortcut } from "./BrowseSection";
import { ALL_COLLAPSED, type ExpandedPlaces } from "./Rail";
import {
  applyFilters,
  DEFAULT_FILTER,
  isFilesPlace,
  type DeskMembership,
  type FilesFilter,
} from "./filters";
import { foldFolderTree } from "./fold";
import { rowString } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../kit/rows";
import { foldFolderLinkStates, linkStateOf, type LinkState } from "./links";
import { artifactFromRow, folderFromRow, isContentKind, type ArtifactRow } from "./rows";
import {
  DEFAULT_FILES_SETTINGS,
  LocalFilesSettingsStore,
  type FilesSettings,
  type FilesSettingsStore,
} from "./settings";
import { useLibraryFeeds } from "./useLibrary";
import { useUploadTasks } from "./useUploadTasks";

// Files: the Library on the desktop (epic #4721). A live folder tree over the
// caller's content-bearing rows, a list that announces changes once and
// quietly, and an inspector that tells each file's provenance story.
//
// ===========================================================================
// TWO FEEDS, ONE SELECTION, EVERY SURFACE A READING OF THEM
// ===========================================================================
// The artifacts and folders collections are retained HERE, at the app root,
// and every surface -- the rail, the list, the inspector, the archive walk --
// is a client-side fold over their snapshots. A second subscription over
// either concept would be one that can disagree (the Deployables rule), and
// a server-side folder filter could not see pre-field rows at all: a row
// promoted before folders existed has no `folderId` member, and only a
// client-side fold reads absence and "" as the same answer, the root.

/** The concepts this app owns, for its Logs section: the index, the bytes,
 *  the folders and the arrangements that fill them. */
const FILES_LOG_CONCEPTS = [
  Concepts.LIBRARY_ARTIFACT,
  Concepts.LIBRARY_FILE,
  Concepts.LIBRARY_FOLDER,
  Concepts.LIBRARY_WATCHED_FOLDER,
] as const;

export function FilesApp({
  sectionId,
  askContext,
  intent,
  consumeIntent,
  store,
  uploads,
}: OsAppProps & { store?: FilesSettingsStore; uploads?: UploadProvider }) {
  // Injectable for tests; nothing in the shell passes either.
  const settingsStore = useMemo(() => store ?? new LocalFilesSettingsStore(), [store]);
  const [settings, setSettings] = useState<FilesSettings>(() => settingsStore.load());
  const authSource = useAuthSource();
  const connection = useOsConnection();
  const { state: osState } = useOs();
  const provider = useMemo(
    () => uploads ?? new EdgeUploadProvider(() => authSource.bearer()),
    [uploads, authSource],
  );

  const { artifacts, folders, files } = useLibraryFeeds();

  // The backups feed is RETAINED AT THE APP ROOT like its three siblings, not
  // inside the section, so switching away and back does not tear the
  // subscription down and re-seed. `useLiveCollection` retains inside an
  // effect, and a hook cannot be called conditionally anyway -- the section
  // switch below returns early.
  const backupsFeed = useBackupsFeed();
  const backups = useLiveView<Row, BackupRow>(backupsFeed.source, "backups", (rows) =>
    rows.map(backupFromRow).filter((backup) => backup.id !== "" && !backup.archived),
  );

  // The tree flow's folder port: one Library mutation, the id minted here
  // (the mutationCreateSpace pattern). The live feed delivers the row.
  const createFolder = useCallback(
    async (name: string, parentFolderId: string): Promise<string> => {
      const query = connection?.query ?? null;
      if (query === null) throw new Error("Not connected to the cluster, so no folder was created.");
      const folderId = newShortId();
      await query.createLibraryFolder({
        folderId,
        name,
        ...(parentFolderId !== "" ? { parentFolderId } : {}),
      });
      return folderId;
    },
    [connection],
  );
  const { tasks, uploadFiles, uploadTree } = useUploadTasks(provider, createFolder);

  const [filter, setFilter] = useState<FilesFilter>(() => ({
    ...DEFAULT_FILTER,
    sortAscending: settings.defaultSort === "oldest",
  }));
  const [selectedId, setSelectedId] = useState("");
  // Which rail places are open. HELD HERE rather than in the browse, for the
  // same reason the filter is: the section switch below returns early, so a
  // trip to Settings and back would otherwise shut everything the person had
  // just opened. Starts all-shut -- Rail.tsx says why.
  const [expanded, setExpanded] = useState<ExpandedPlaces>(ALL_COLLAPSED);

  // ===========================================================================
  // THE DESK, FOLDED FOR THE DESKTOP PLACE (epic memql#4842, #4846)
  // ===========================================================================
  // What sits on the desks is SHELL state (the roamed desktop document), read
  // here and folded into the pure shapes the filter and the rail consume:
  // loose file icons by artifact id (uploads in flight excluded -- an icon
  // with no artifact yet is desk-only), folder shortcuts deduped by folderId,
  // and which desk holds each artifact so a row can say "Desk 2" when more
  // than one desk holds items.
  const desk = useMemo(() => {
    const fileArtifactIds = new Set<string>();
    const folderIds = new Set<string>();
    const folderShortcuts = new Map<string, string>();
    const deskIndexByArtifactId = new Map<string, number>();
    const desksWithItems = new Set<string>();
    osState.shell.desks.forEach((d, index) => {
      const surface = osState.surfaces[d.id];
      if (!surface) return;
      for (const item of Object.values(surface.items)) {
        if (item.kind === "file" && item.artifactId !== "") {
          fileArtifactIds.add(item.artifactId);
          if (!deskIndexByArtifactId.has(item.artifactId)) {
            deskIndexByArtifactId.set(item.artifactId, index);
          }
          desksWithItems.add(d.id);
        } else if (item.kind === "folder") {
          folderIds.add(item.folderId);
          if (!folderShortcuts.has(item.folderId)) folderShortcuts.set(item.folderId, item.name);
          desksWithItems.add(d.id);
        }
      }
    });
    const shortcuts: DeskFolderShortcut[] = [...folderShortcuts.entries()]
      .map(([folderId, name]) => ({ folderId, name }))
      .sort((a, b) => a.name.localeCompare(b.name));
    const membership: DeskMembership = { fileArtifactIds, folderIds };
    return { membership, shortcuts, deskIndexByArtifactId, desksWithItems: desksWithItems.size };
  }, [osState.shell.desks, osState.surfaces]);

  // The open intent (epic memql#4842, #4845): "show this place, this folder".
  // Consumed by id, so acting on a stale render can never eat a newer
  // instruction; an unrecognized payload is consumed and ignored rather than
  // left standing to re-fire on every render.
  useEffect(() => {
    if (!intent) return;
    const place = intent.payload.place;
    const folderId = intent.payload.folderId;
    if (isFilesPlace(place)) {
      setFilter((f) => ({
        ...f,
        place,
        folderId: typeof folderId === "string" ? folderId : "",
        search: "",
      }));
      // Arriving by intent OPENS the place. Being sent here from a desk icon
      // is the same request as clicking the place in the rail, and answering
      // it with a shut disclosure would hide the folder the sender named.
      setExpanded((e) => ({ ...e, [place]: true }));
      setSelectedId("");
    }
    consumeIntent?.(intent.id);
  }, [intent, consumeIntent]);

  // The list's own reading of the artifacts feed: project, then narrow, then
  // order, in one pass -- the collection holds RAW wire rows, so every
  // predicate runs on an `artifactFromRow` result. The viewKey is the filter
  // written down: a changed filter MEANS a different reading -- and the desk
  // membership joins it, because a shortcut appearing reveals rows the
  // browser already had, which must re-baseline rather than ring.
  const deskKey = `${[...desk.membership.fileArtifactIds].sort().join(",")}~${[...desk.membership.folderIds].sort().join(",")}`;
  const filterKey = [
    filter.place,
    filter.folderId ?? "~",
    filter.kind,
    filter.source,
    filter.accountId,
    filter.search,
    filter.sortAscending ? "asc" : "desc",
    filter.place === "desktop" ? deskKey : "",
  ].join("|");
  const list = useLiveView<Row, ArtifactRow>(artifacts.source, `files:list:${filterKey}`, (rows) =>
    applyFilters(
      rows.map(artifactFromRow).filter((r) => r.id !== ""),
      filter,
      desk.membership,
    ),
  );

  // The whole content population, unfiltered -- what "empty" and the rail's
  // counts are honestly about.
  const content = useMemo(
    () =>
      artifacts.snapshot.rows
        .map(artifactFromRow)
        .filter((r) => r.id !== "" && isContentKind(r.kind)),
    [artifacts.snapshot],
  );

  // The origin link states (epic memql#4783), keyed by the file id the index
  // row points at. Folded from the file feed rather than read per row: one
  // pass over a snapshot the app already holds, and the rollup below needs
  // every file anyway.
  const linkByFileId = useMemo(() => {
    const byId = new Map<string, LinkState | "">();
    for (const raw of files.snapshot.rows) {
      const row = flatten(raw);
      const id = rowString(row, "id").split(":").pop() ?? "";
      if (id !== "") byId.set(id, linkStateOf(raw));
    }
    return byId;
  }, [files.snapshot]);

  // The tree, from the folders feed. Archived folders are dropped HERE, not
  // by the read: the seed now includes them for the Archive place, and an
  // archive flip arrives as an UPDATE the fold has to keep answering for.
  //
  // DELETED folders are dropped here too, and for a sharper reason. No folder
  // read returns one, so a seed never carries one -- but a live re-read is the
  // raw by-id fetch, which applies row authz and no query filter, so the row
  // comes back on the very update that deleted it. Without this conjunct the
  // folder would stay in the tree until somebody reloaded, which reads as the
  // delete having done nothing at all.
  const tree = useMemo(
    () =>
      foldFolderTree(
        folders.snapshot.rows
          .map(folderFromRow)
          .filter((f) => f.id !== "" && !f.archived && !f.deleted),
      ),
    [folders.snapshot],
  );

  // The Archive place's folders: flat and alphabetical (epic memql#4842,
  // #4846). Deliberately not a tree -- archived folders' ancestry mixes live
  // and archived parents, and a tree over that would lie one way or the
  // other. Flat, named, counted is the honest reading.
  //
  // `!f.deleted` for the live-path reason above, and it is not redundant with
  // `f.archived`: the two are independent fields, so a folder somebody
  // archived before the empty-folder rule existed carries BOTH the day the
  // walk deletes it, and this place is one of the two it must leave.
  const archivedFolders = useMemo(
    () =>
      folders.snapshot.rows
        .map(folderFromRow)
        .filter((f) => f.id !== "" && f.archived && !f.deleted)
        .sort((a, b) => a.name.localeCompare(b.name)),
    [folders.snapshot],
  );

  // The folder badges: the WORST state anywhere beneath each folder. Computed
  // over the artifact index rather than the file rows, because filing lives on
  // the INDEX -- a file row's own folderId is the initial filing only and a
  // later move deliberately never comes back to it.
  const folderLinks = useMemo(
    () =>
      foldFolderLinkStates(
        content.map((row) => ({
          folderId: row.folderId,
          state:
            row.kind === "file"
              ? (linkByFileId.get(row.sourceConceptRef.split(":").pop() ?? "") ?? "")
              : "",
        })),
        (folderId) => tree.byId.get(folderId)?.folder.parentFolderId ?? "",
      ),
    [content, linkByFileId, tree],
  );

  function update(patch: Partial<FilesSettings>) {
    const next = { ...settings, ...patch, version: 1 as const };
    setSettings(next);
    settingsStore.save(next);
    // Settings take effect without reopening the window: the browse follows
    // the new defaults now. The session's own controls keep steering
    // afterwards -- a default is where a session starts, and this is the one
    // moment "starts" is re-read.
    setFilter((f) => ({
      ...f,
      sortAscending: next.defaultSort === "oldest",
    }));
  }

  if (sectionId === "settings") {
    return <FilesSettingsSection settings={settings} update={update} />;
  }
  if (sectionId === "logs") {
    return (
      <AppLogsSection
        app="files"
        subjectConcepts={FILES_LOG_CONCEPTS}
        intent={intent}
        consumeIntent={consumeIntent}
      />
    );
  }
  if (sectionId === "backups") {
    return (
      <BackupsSection
        folders={folders.snapshot.rows}
        files={files.snapshot.rows}
        source={backups}
      />
    );
  }
  return (
    <BrowseSection
      list={list}
      artifacts={artifacts}
      foldersState={folders.snapshot.state}
      tree={tree}
      content={content}
      archivedFolders={archivedFolders}
      deskFolders={desk.shortcuts}
      deskFileArtifactIds={desk.membership.fileArtifactIds}
      deskIndexByArtifactId={desk.deskIndexByArtifactId}
      desksWithItems={desk.desksWithItems}
      filter={filter}
      setFilter={setFilter}
      expanded={expanded}
      setExpanded={setExpanded}
      selectedId={selectedId}
      onSelect={setSelectedId}
      linkByFileId={linkByFileId}
      folderLinks={folderLinks}
      confirmBeforeArchive={settings.confirmBeforeArchive}
      askContext={askContext}
      tasks={tasks}
      uploadFiles={uploadFiles}
      uploadTree={uploadTree}
      uploads={provider}
    />
  );
}

function FilesSettingsSection({
  settings,
  update,
}: {
  settings: FilesSettings;
  update: (patch: Partial<FilesSettings>) => void;
}) {
  return (
    <div className="os-settings">
      <Head title="Files settings" />
      <Panel label="Files settings">
        <fieldset className="os-field-group">
          <legend>Open the list on</legend>
          <div className="os-choice-row" role="radiogroup" aria-label="Default sort">
            {(["newest", "oldest"] as const).map((sort) => (
              <button
                key={sort}
                type="button"
                role="radio"
                aria-checked={settings.defaultSort === sort}
                className="os-choice"
                onClick={() => update({ defaultSort: sort })}
              >
                {sort === "newest" ? "Newest first" : "Oldest first"}
              </button>
            ))}
          </div>
          <p className="os-caption">
            Where a Files window starts. The sort control in the browse toolbar keeps steering the
            window you are in.
          </p>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>Archiving</legend>
          <Check
            checked={settings.confirmBeforeArchive}
            onChange={(confirmBeforeArchive) => update({ confirmBeforeArchive })}
          >
            Ask before archiving
          </Check>
          <p className="os-caption">
            The confirm names what is about to move -- for a folder, the live count of everything
            inside it. Archiving never deletes: everything archived lives under Archive in the
            browse rail, and in the Bin, where it can be put back.
          </p>
        </fieldset>

        <p className="os-caption">
          These are kept in this browser, separately from your desktop, so an app learning a
          preference can never cost you your desks. The defaults are{" "}
          {DEFAULT_FILES_SETTINGS.defaultSort} first, asking before archive.
        </p>
      </Panel>
    </div>
  );
}
