import { useCallback, useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { Folder as FolderIcon } from "lucide-react";

import { Caption, Check, Chip, Head, LiveList, Panel, Refine, Row as KitRow, formatFreshness, formatMoment, useNow } from "../../kit";
import { useOsConnection } from "../../live/connection";
import { useMachines } from "../../live/machines";
import type { OsAppProps } from "../../system/registry";
import { kindGlyph } from "../files/BrowseSection";
import { artifactFromRow, folderFromRow, isContentKind, fileStory } from "../files/rows";
import type { ArtifactRow } from "../files/rows";
import {
  DEFAULT_FILES_SETTINGS,
  LocalFilesSettingsStore,
  type FilesSettings,
  type FilesSettingsStore,
} from "../files/settings";
import { BinDetail } from "./BinDetail";
import { useTwoFeedView } from "../../live/mergedView";
import { planRestore, runRestore } from "./restore";
import { binFingerprint, binItemFromArtifact, binItemFromFolder, filterBinItems, orderBinItems, type BinItem } from "./rows";
import { useBinFeeds } from "./useBin";

// The Bin: MemQL OS's archive, docked permanently and unable to destroy
// anything (memql#4784).
//
// ===========================================================================
// THIS IS NOT A COUNTDOWN, AND THE APP SAYS SO ON THE PAGE
// ===========================================================================
// Every trash can anybody has used is a waiting room for deletion: things sit
// in it until they are gone, and the interesting number is how long they have
// left. Nothing here works that way. An archive in MemQL is an append-only
// re-version carrying archived=true -- the bytes stay, every earlier version
// stays, the provenance stays -- and there is no expiry, no quota sweep and no
// cleanup pass anywhere in this product.
//
// A surface that looked like a trash can and quietly behaved differently would
// be read as one, so the difference is stated rather than implied: in the
// header line, in the empty state, and in the settings section where somebody
// goes looking for the retention control that does not exist. Retention is
// explicitly out of scope for this issue and will be specified later; until it
// is, saying nothing would be the design making a promise the engine does not
// keep.

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export function BinApp({ sectionId, askContext, store }: OsAppProps & { store?: FilesSettingsStore }) {
  // The Files store, not one of this app's own: "ask before archiving" is ONE
  // setting with two doors, and a second copy of it is a second answer.
  const settingsStore = useMemo(() => store ?? new LocalFilesSettingsStore(), [store]);
  const [settings, setSettings] = useState<FilesSettings>(() => settingsStore.load());

  const connection = useOsConnection();
  const { presence } = useMachines();
  // One clock for every relative time in the list, so two rows a second apart
  // never disagree about what "now" is.
  const now = useNow(30_000);
  const { artifacts, folders } = useBinFeeds();

  const [selectedId, setSelectedId] = useState("");
  const [search, setSearch] = useState("");
  const [restoring, setRestoring] = useState("");
  const [restoreError, setRestoreError] = useState("");

  // The archived folders, by id -- what "was filed in" resolves against. It
  // has to be THIS feed rather than the Files tree: libraryFolders carries
  // `archived != true`, so a folder that went to the Bin with its contents is
  // invisible to every other surface in the product, and an item filed in one
  // would render "Library (top level)" -- which is a different, wrong answer.
  const folderNames = useMemo(() => {
    const names = new Map<string, string>();
    for (const raw of folders.snapshot.rows) {
      const row = folderFromRow(raw);
      if (row.id !== "") names.set(row.id, row.name.trim() || row.id);
    }
    return names;
  }, [folders.snapshot]);

  const folderNameOf = useCallback(
    (folderId: string) => {
      if (folderId.trim() === "") return "Library (top level)";
      return folderNames.get(folderId) ?? "a folder that is no longer here";
    },
    [folderNames],
  );

  // Both feeds, projected and merged into one ordered list. The merge is the
  // whole reason the order is computed here: folders and artifacts arrive on
  // separate reads, and a Bin that listed every folder and then every file
  // would bury the thing somebody just threw away in the middle of it.
  const viewKey = `bin:${search.trim().toLowerCase()}`;
  const list = useTwoFeedView<Row, Row, BinItem>(
    artifacts.source,
    folders.source,
    viewKey,
    (artifactRows, folderRows) => {
      const items = artifactRows
        .map(artifactFromRow)
        .filter((r) => r.id !== "" && isContentKind(r.kind))
        .map(binItemFromArtifact);
      // `!f.deleted` is the live-path guard, not a second copy of the read's.
      // libraryArchivedFolders carries isNotDeleted, so a seed never brings a
      // deleted folder here -- but a live re-read is the raw by-id fetch, with
      // row authz and no query filter, so a folder that was ALREADY in the Bin
      // when the archive walk deleted it comes back on that very update.
      // Without this it would sit in the Bin offering a Restore that the reads
      // will never honour, which is the exact noise the disposition removes.
      const folderItems = folderRows
        .map(folderFromRow)
        .filter((f) => f.id !== "" && !f.deleted)
        .map(binItemFromFolder);
      return filterBinItems(orderBinItems([...items, ...folderItems]), search);
    },
  );

  // The selected item's full index row, for the facts the projection drops.
  // Read off the feed the list already holds -- one source of truth for the
  // row, rather than a second by-id read that could disagree with it.
  const artifactById = useMemo(() => {
    const byId = new Map<string, ArtifactRow>();
    for (const raw of artifacts.snapshot.rows) {
      const row = artifactFromRow(raw);
      if (row.id !== "") byId.set(row.id, row);
    }
    return byId;
  }, [artifacts.snapshot]);

  const selected = useMemo(
    () => (list?.snapshot.rows ?? []).find((item) => item.id === selectedId) ?? null,
    [list, selectedId],
  );

  const restore = useCallback(
    async (item: BinItem) => {
      const query = connection?.query ?? null;
      if (query === null) {
        setRestoreError("Not connected to the cluster, so nothing was restored.");
        return;
      }
      setRestoring(item.id);
      setRestoreError("");
      try {
        // Nothing is patched locally: the un-archive broadcasts, and the row
        // leaves this list and reappears in Files on the same feed.
        await runRestore(planRestore(item), {
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
      } catch (err: unknown) {
        setRestoreError(describe(err));
      } finally {
        setRestoring("");
      }
    },
    [connection],
  );

  function update(patch: Partial<FilesSettings>) {
    const next = { ...settings, ...patch, version: 1 as const };
    setSettings(next);
    settingsStore.save(next);
  }

  if (sectionId === "settings") {
    return <BinSettingsSection settings={settings} update={update} />;
  }

  return (
    <div className="os-bin">
      <Head title="Bin">
        {/* The search rides the Refine affordance (DESIGN.md rule 2) --
            collapsed until asked, never standing chrome over the list. */}
        <Refine
          search={search}
          onSearch={setSearch}
          placeholder="Name, label or machine"
          label="Search the Bin"
        />
      </Head>

      {/* THE INVARIANT, ON THE PAGE. Not a tooltip and not a settings note:
          it is the one thing about this surface that differs from every
          trash can the reader has used, and a person who never opens
          settings still has to know it. */}
      <p className="os-bin-lede">
        Nothing here has been deleted. Archived items keep their bytes, their whole version history
        and everywhere they came from, and stay until you restore them.
      </p>

      <div className="os-bin-body">
        <div className="os-bin-list">
          <LiveList<BinItem>
            source={list}
            label="Archived items"
            rowId={(item) => item.id}
            fingerprint={binFingerprint}
            emptyText={
              search.trim() === ""
                ? "The Bin is empty. Archiving a file from Files sends it here -- it keeps its bytes and its history, and waits for you."
                : "Nothing in the Bin matches that."
            }
            renderRow={(item) => (
              <BinRow
                item={item}
                artifact={artifactById.get(item.id) ?? null}
                filedIn={folderNameOf(item.folderId)}
                presence={presence}
                now={now}
                selected={item.id === selectedId}
                onOpen={() => {
                  setSelectedId(item.id);
                  setRestoreError("");
                  askContext(`bin item:${item.name}`);
                }}
              />
            )}
          />
          <Caption>
            Newest first -- an archive re-writes the row, so for most items this is the order they
            were archived in.
          </Caption>
        </div>

        {selected === null ? null : (
          <BinDetail
            item={selected}
            artifact={artifactById.get(selected.id) ?? null}
            folderNameOf={folderNameOf}
            presence={presence}
            restoring={restoring === selected.id}
            restoreError={restoreError}
            onRestore={() => void restore(selected)}
            onClose={() => setSelectedId("")}
          />
        )}
      </div>
    </div>
  );
}

function BinRow({
  item,
  artifact,
  filedIn,
  presence,
  now,
  selected,
  onOpen,
}: {
  item: BinItem;
  artifact: ArtifactRow | null;
  filedIn: string;
  presence: (workerId: string) => { name?: string; online: boolean } | null;
  now: Date;
  selected: boolean;
  onOpen: () => void;
}) {
  // THE ROW'S QUIET MIDDLE IS THE PROVENANCE, not the date. "cut-03.mov, from
  // Studio-MacBook" is what somebody deciding whether they still want it
  // reads; the moment is on the trailing edge with the rest of the state,
  // where the other lists in this shell put theirs.
  //
  // AND THERE IS NO PRESENCE DOT HERE, which is a deliberate subtraction. The
  // dot means "is that machine reachable right now" everywhere in this shell,
  // and beside an archived row that is both irrelevant -- the question is
  // whether you still want this, not whether the laptop is awake -- and
  // actively misleading: a file whose origin is GONE renders a green dot for
  // as long as the machine that no longer holds it stays online. The detail
  // panel keeps the dot, where it sits against the sentence that qualifies it.
  const story = artifact === null ? null : fileStory(artifact, artifact.producedByWorkerId ? presence(artifact.producedByWorkerId) : null);
  return (
    <KitRow
      icon={
        <span className="os-bin-row-glyph">
          {item.kind === "folder" ? <FolderIcon size={16} aria-hidden /> : kindGlyph(item.contentKind, 16)}
        </span>
      }
      name={item.name}
      onOpen={onOpen}
      open={selected}
      dim={!selected}
      state={
        item.changedAt === "" ? null : (
          <span className="os-caption" title={formatMoment(item.changedAt)}>
            {formatFreshness(item.changedAt, now)}
          </span>
        )
      }
    >
      {/* A folder's provenance is that somebody made it here -- parallel to
          "Uploaded here", and the honest answer for a row with no bytes. */}
      <span className="os-bin-row-from">{story?.sentence ?? "Made here"}</span>
      <Chip tone="muted" title={`Was filed in ${filedIn}`}>
        {filedIn}
      </Chip>
    </KitRow>
  );
}

function BinSettingsSection({
  settings,
  update,
}: {
  settings: FilesSettings;
  update: (patch: Partial<FilesSettings>) => void;
}) {
  return (
    <div className="os-settings">
      <Head title="Bin settings" />
      <Panel label="Bin settings">
        <fieldset className="os-field-group">
          <legend>Archiving</legend>
          <Check
            checked={settings.confirmBeforeArchive}
            onChange={(confirmBeforeArchive) => update({ confirmBeforeArchive })}
          >
            Ask before archiving
          </Check>
          <p className="os-caption">
            The same setting the Files app carries, not a second copy of it -- changing it here
            changes it there. It covers every route into the Bin: the row action, the folder walk,
            and dropping something onto the Bin in the dock.
          </p>
        </fieldset>

        {/* THE ABSENT CONTROL, WITH AN ACCOUNT OF ITSELF. Somebody who opens
            settings in a trash can is usually looking for "empty after 30
            days". There is no such control, and an absent one with nothing
            said about it reads as something nobody got round to building. */}
        <fieldset className="os-field-group">
          <legend>How long things stay</legend>
          <p className="os-caption">
            Indefinitely. There is no automatic cleanup, no expiry and no size limit on the Bin:
            archived items accumulate and are kept until you do something about them. Restoring an
            item puts it back; nothing in this product removes one for you.
          </p>
        </fieldset>

        <p className="os-caption">
          Archiving is never a delete. An archived item is a new version of the same row carrying
          "archived", so its bytes, its earlier versions and its provenance all survive it --
          which is also why restoring is simply the opposite flag rather than a rebuild.
        </p>
      </Panel>
      <Caption>
        Defaults: {DEFAULT_FILES_SETTINGS.confirmBeforeArchive ? "ask" : "do not ask"} before
        archiving.
      </Caption>
    </div>
  );
}
