import { useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { Check, Head, Panel } from "../../kit";
import { useLiveView } from "../../live/liveView";
import type { OsAppProps } from "../../system/registry";
import { BrowseSection } from "./BrowseSection";
import { applyFilters, DEFAULT_FILTER, type FilesFilter } from "./filters";
import { foldFolderTree } from "./fold";
import { artifactFromRow, folderFromRow, isContentKind, type ArtifactRow } from "./rows";
import {
  DEFAULT_FILES_SETTINGS,
  LocalFilesSettingsStore,
  type FilesSettings,
  type FilesSettingsStore,
} from "./settings";
import { useLibraryFeeds } from "./useLibrary";

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

export function FilesApp({
  sectionId,
  askContext,
  store,
}: OsAppProps & { store?: FilesSettingsStore }) {
  // Injectable for tests; nothing in the shell passes one.
  const settingsStore = useMemo(() => store ?? new LocalFilesSettingsStore(), [store]);
  const [settings, setSettings] = useState<FilesSettings>(() => settingsStore.load());

  const { artifacts, folders } = useLibraryFeeds();

  const [filter, setFilter] = useState<FilesFilter>(() => ({
    ...DEFAULT_FILTER,
    sortAscending: settings.defaultSort === "oldest",
    showArchived: settings.showArchived,
  }));
  const [selectedId, setSelectedId] = useState("");

  // The list's own reading of the artifacts feed: project, then narrow, then
  // order, in one pass -- the collection holds RAW wire rows, so every
  // predicate runs on an `artifactFromRow` result. The viewKey is the filter
  // written down: a changed filter MEANS a different reading.
  const filterKey = [
    filter.folderId ?? "~",
    filter.kind,
    filter.source,
    filter.showArchived ? "arch" : "",
    filter.search,
    filter.sortAscending ? "asc" : "desc",
  ].join("|");
  const list = useLiveView<Row, ArtifactRow>(artifacts.source, `files:list:${filterKey}`, (rows) =>
    applyFilters(
      rows.map(artifactFromRow).filter((r) => r.id !== ""),
      filter,
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

  // The tree, from the folders feed. Archived folders are dropped HERE as
  // well as by the read's own conjunct, because an archive flip arrives as an
  // UPDATE: the read excludes it and the subscription does not.
  const tree = useMemo(
    () =>
      foldFolderTree(
        folders.snapshot.rows.map(folderFromRow).filter((f) => f.id !== "" && !f.archived),
      ),
    [folders.snapshot],
  );

  function update(patch: Partial<FilesSettings>) {
    const next = { ...settings, ...patch, version: 1 as const };
    setSettings(next);
    settingsStore.save(next);
    // Settings take effect without reopening the window: the browse follows
    // the new defaults now. The toolbar's own toggles keep steering the
    // session afterwards -- a default is where a session starts, and this is
    // the one moment "starts" is re-read.
    setFilter((f) => ({
      ...f,
      sortAscending: next.defaultSort === "oldest",
      showArchived: next.showArchived,
    }));
  }

  if (sectionId === "settings") {
    return <FilesSettingsSection settings={settings} update={update} />;
  }
  return (
    <BrowseSection
      list={list}
      artifacts={artifacts}
      foldersState={folders.snapshot.state}
      tree={tree}
      content={content}
      filter={filter}
      setFilter={setFilter}
      selectedId={selectedId}
      onSelect={setSelectedId}
      confirmBeforeArchive={settings.confirmBeforeArchive}
      askContext={askContext}
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
            inside it. Archiving never deletes: archived files keep their bytes and come back under
            the archived filter.
          </p>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>Archived files</legend>
          <Check checked={settings.showArchived} onChange={(showArchived) => update({ showArchived })}>
            Show archived by default
          </Check>
          <p className="os-caption">
            Archived rows are marked wherever they show. The browse toolbar can flip this per
            window.
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
