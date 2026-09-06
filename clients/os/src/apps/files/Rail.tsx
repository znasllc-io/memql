import { type ReactNode } from "react";
import { ChevronRight, Files, Folder, HardDrive, Monitor, Sparkles, Trash2 } from "lucide-react";
import type { LiveState } from "@znasllc-io/memql-sdk-core/client";

import { Caption, Chip } from "../../kit";
import type { FilesFilter, FilesPlace } from "./filters";
import type { BinRail, FolderTree, MaterializedRail, TreeNode } from "./fold";
import { kindGlyph } from "./glyphs";
import { LINK_LABEL, LINK_SENTENCE, type LinkState } from "./links";
import { artifactName, type ArtifactRow, type FolderRow } from "./rows";
import type { DeskFolderShortcut } from "./BrowseSection";

// The rail: three PLACES, each a disclosure over its own contents.
//
// ===========================================================================
// WHY THE PLACES COLLAPSE, AND WHY THEY START COLLAPSED
// ===========================================================================
// The rail used to render every place's folders at once, always. That is fine
// with four folders and unusable with forty: the three places are the rail's
// actual navigation, and they were being pushed off the bottom by the
// contents of whichever one happened to be biggest. Collapsed-by-default puts
// the three destinations on one screen and makes depth something you ask for.
//
// SELECTING A PLACE ALSO OPENS IT, and that is not a shortcut -- clicking
// "Bin" is the gesture for "show me what is in the Bin", so answering it
// with a scoped list and a still-shut disclosure would make the person click
// the same row twice to mean one thing. The chevron is the other direction:
// it opens and shuts without changing what the list is showing, so somebody
// reading the Library can look at what is on their desks without losing their
// place.
//
// A PLACE COUNTS ONLY WHERE THE NUMBER ANSWERS SOMETHING NOBODY ELSE IS:
// shut, and not the place being looked at. It counts the WHOLE place, since
// the question is "what is in here that I cannot see" -- a shut Library
// summarising as its root folder's count would be a smaller number standing
// in front of a bigger one it does not mention.
//
// The per-FOLDER count stays direct, which is the recorded answer for a
// location (BrowseSection: "what a person would count"); nothing here
// overturns it.
//
// THE LIBRARY AND DESKTOP RAILS LIST FOLDERS, NOT FILES. Files are what the
// list beside them is for, and a second copy of them in a 184px column would
// be the same rows twice, narrower. Opening a place scopes the list; that is
// where its files appear.
//
// THE BIN IS THE ONE EXCEPTION, and it is an exception to the REASON rather
// than to the rule: in the Bin the rail is not showing the same rows the list
// is. `fold.ts`'s `foldBinRail` header carries the three-part argument; the
// short form is that archived folders are a flat ancestry-less set, most
// archived files are loose, and a folders-only Bin therefore expanded to
// "Nothing archived." while forty archived files sat in the list beside it.

/** Which places are open. Held by the app root so switching to Settings and
 *  back does not shut everything the person just opened. */
export type ExpandedPlaces = Record<FilesPlace, boolean>;

export const ALL_COLLAPSED: ExpandedPlaces = {
  library: false,
  desktop: false,
  bin: false,
  materializer: false,
};

export function Rail({
  filter,
  patch,
  tree,
  counts,
  folderLinks,
  bin,
  materializedRail,
  openBinFolders,
  setOpenBinFolders,
  deskFolders,
  folderNameOf,
  libraryTotal,
  deskFileCount,
  expanded,
  setExpanded,
  renamingFolderId,
  onRename,
  onCancelRename,
  onFolderMenu,
  onArchivedFolderMenu,
  onSelectBinFile,
  selectedFileId,
  railNote,
  foldersState,
}: {
  filter: FilesFilter;
  patch: (next: Partial<FilesFilter>) => void;
  tree: FolderTree;
  counts: Map<string, number>;
  folderLinks: Map<string, LinkState>;
  /** The Bin's picture: its folders with their files, and what is loose. */
  bin: BinRail;
  /** The Materializer place's picture: the folders its outputs are filed in. */
  materializedRail: MaterializedRail;
  /** Which Bin folders are open, held by the app root for the reason
   *  `expanded` is -- a trip to Settings must not shut them. */
  openBinFolders: ReadonlySet<string>;
  /**
   * FUNCTIONAL ONLY, and the signature says so rather than trusting the
   * caller. Two disclosures flipped inside one React batch both read the
   * SAME rendered `openBinFolders`, so a value-taking setter applies the
   * second over the first and one chevron silently does nothing. Found by
   * looking at a screenshot -- every vitest case clicks once per render, so
   * the whole suite was green over it.
   */
  setOpenBinFolders: (update: (prev: ReadonlySet<string>) => ReadonlySet<string>) => void;
  deskFolders: DeskFolderShortcut[];
  folderNameOf: (folderId: string) => string;
  /** Everything live in the Library -- the collapsed summary's honest number. */
  libraryTotal: number;
  deskFileCount: number;
  expanded: ExpandedPlaces;
  /**
   * FUNCTIONAL ONLY, for the reason `setOpenBinFolders` is below -- and this
   * one was already wrong before the Bin's disclosures existed. Two place
   * chevrons flipped inside one React batch both read the SAME rendered
   * `expanded`, so the second applied over the first and one place silently
   * stayed shut. Found in a screenshot of three places being opened at once;
   * every vitest case clicks one per render.
   */
  setExpanded: (update: (prev: ExpandedPlaces) => ExpandedPlaces) => void;
  renamingFolderId: string;
  onRename: (folderId: string, name: string) => void;
  onCancelRename: () => void;
  onFolderMenu: (x: number, y: number, node: TreeNode) => void;
  onArchivedFolderMenu: (x: number, y: number, folder: FolderRow) => void;
  /**
   * Show one archived file: scope the list so it is in it, and select it.
   * The second argument is the scope it was found under -- the folder's id,
   * or "" for the loose group, which in the Bin already means everything
   * archived. The RAIL knows this and the caller would have to re-derive it.
   */
  onSelectBinFile: (row: ArtifactRow, folderId: string) => void;
  /** The selected row, so the Bin's file rows can mark the current one. */
  selectedFileId: string;
  railNote: string;
  foldersState: LiveState;
}) {
  const searching = filter.search.trim() !== "";
  const open = (place: FilesPlace) => setExpanded((prev) => ({ ...prev, [place]: true }));
  const toggle = (place: FilesPlace) =>
    setExpanded((prev) => ({ ...prev, [place]: !prev[place] }));
  const go = (place: FilesPlace, folderId: string) => {
    patch({ place, folderId, search: "" });
    open(place);
  };
  // The Bin's own disclosures, one level in. Keyed by folder id, with one
  // reserved key for the loose group -- a folder id is a short id and can
  // never collide with it.
  /** Standing in the Bin's own root -- where the Head and the list are
   *  already naming and counting this place. */
  const inBin = !searching && filter.place === "bin" && filter.folderId === "";
  const flip = (key: string) =>
    setOpenBinFolders((prev) => {
      const next = new Set(prev);
      if (!next.delete(key)) next.add(key);
      return next;
    });

  return (
    <nav className="os-files-rail" aria-label="Places and folders">
      <Place
        place="library"
        glyph={<HardDrive size={14} aria-hidden />}
        name="Library"
        count={libraryTotal}
        countTitle={`${libraryTotal} in your Library`}
        current={!searching && filter.place === "library" && filter.folderId === ""}
        expanded={expanded.library}
        onToggle={() => toggle("library")}
        onSelect={() => go("library", "")}
        emptyText="No folders yet."
        empty={tree.roots.length === 0}
      >
        {tree.roots.map((node) => (
          <RailNode
            key={node.folder.id}
            node={node}
            counts={counts}
            folderLinks={folderLinks}
            currentId={searching || filter.place !== "library" ? null : filter.folderId}
            renamingFolderId={renamingFolderId}
            onScope={(folderId) => go("library", folderId)}
            onMenu={onFolderMenu}
            onRename={onRename}
            onCancelRename={onCancelRename}
          />
        ))}
      </Place>

      <Place
        place="desktop"
        glyph={<Monitor size={14} aria-hidden />}
        name="Desktop"
        count={deskFileCount}
        countTitle={`${deskFileCount} on your desks`}
        current={!searching && filter.place === "desktop" && filter.folderId === ""}
        expanded={expanded.desktop}
        onToggle={() => toggle("desktop")}
        onSelect={() => go("desktop", "")}
        emptyText="No folders on your desks."
        empty={deskFolders.length === 0}
      >
        {deskFolders.map((shortcut) => (
          <button
            key={shortcut.folderId}
            type="button"
            className="os-files-node"
            data-current={
              !searching && filter.place === "desktop" && filter.folderId === shortcut.folderId
                ? true
                : undefined
            }
            onClick={() => go("desktop", shortcut.folderId)}
          >
            <Folder size={14} aria-hidden />
            <span className="os-files-node-name">{folderNameOf(shortcut.folderId)}</span>
            <span className="os-files-node-count">
              {(counts.get(shortcut.folderId) ?? 0) > 0 ? counts.get(shortcut.folderId) : ""}
            </span>
          </button>
        ))}
      </Place>

      {/* THE BIN. The glyph is the dock fixture's own `Trash2` and the name is
          the dock fixture's own word: the row menu has always said "Move to
          Bin", so a rail that answered "Archive" made one destination look
          like two.

          ITS EMPTY LINE FOLLOWS THE RULE ITS COUNT FOLLOWS -- it answers only
          where nobody else is. This place's group IS the whole place, so
          standing in an empty Bin put "The Bin is empty." in the rail and
          "The Bin is empty. Archiving from the Library keeps files here, not
          deleted." in the list, 200px apart (DESIGN.md rule 7). Library's and
          Desktop's lines stay in both states, because theirs are about
          FOLDERS and their lists' are about files: two statements, not one
          said twice. */}
      <Place
        place="bin"
        glyph={<Trash2 size={14} aria-hidden />}
        name="Bin"
        count={bin.fileCount + bin.folderCount}
        countTitle={binSummary(bin)}
        current={inBin}
        expanded={expanded.bin}
        onToggle={() => toggle("bin")}
        onSelect={() => go("bin", "")}
        emptyText={inBin ? "" : "The Bin is empty."}
        empty={bin.folders.length === 0 && bin.loose.length === 0}
        groupClass="os-files-bin-group"
      >
        {bin.folders.map((node) => (
          <BinGroup
            key={node.folder.id}
            id={node.folder.id}
            glyph={<Folder size={14} aria-hidden />}
            name={node.folder.name}
            files={node.files}
            emptyText="Nothing filed here is in the Bin."
            current={!searching && filter.place === "bin" && filter.folderId === node.folder.id}
            selectedFileId={searching ? "" : selectedFileId}
            expanded={openBinFolders.has(node.folder.id)}
            onToggle={() => flip(node.folder.id)}
            onScope={() => go("bin", node.folder.id)}
            onMenu={(x, y) => onArchivedFolderMenu(x, y, node.folder)}
            onSelectFile={(row) => onSelectBinFile(row, node.folder.id)}
          />
        ))}
        {/* WHAT IS NOT IN A FOLDER IS A GROUP, NOT A ROW PER FILE.
            Listing every loose file at the place's own level would put the
            list's own rows in the rail beside it, narrower -- the exact
            duplication the folders-not-files rule exists to prevent. A shut
            group answers the question the old rail could not ("how much is
            in here that I cannot see") in one honest number, and expanding
            it is a thing the person asks for.

            It is NOT drawn as a folder: it is a set, nothing can be restored
            to it, and a folder glyph would say otherwise. */}
        {bin.loose.length > 0 ? (
          <BinGroup
            id={LOOSE_GROUP}
            glyph={<Files size={14} aria-hidden />}
            name="Not in a folder"
            files={bin.loose}
            emptyText=""
            current={false}
            selectedFileId={searching ? "" : selectedFileId}
            expanded={openBinFolders.has(LOOSE_GROUP)}
            onToggle={() => flip(LOOSE_GROUP)}
            onScope={null}
            onMenu={null}
            onSelectFile={(row) => onSelectBinFile(row, "")}
          />
        ) : null}
      </Place>

      {/* THE MATERIALIZER (epic memql#4981, #4983). Named for the app, the
          way the Bin is named for the app -- a person who made a file in the
          Materializer looks for "Materializer".

          ITS RAIL LISTS FOLDERS, NOT FILES, and that is not an inconsistency
          with the Bin one place up: the Bin earned its exception by having
          almost no navigation to list, while this place has the LIBRARY's
          shape -- ordinary files in ordinary folders -- so opening it scopes
          the list to precisely the rows the rail would otherwise repeat.

          THE PLACE IS PERMANENT, EMPTY OR NOT. The three above it are
          locations rather than results, and a fourth that came and went with
          the data would make the rail's shape depend on what happens to be in
          it. Its empty state is where a person finds out what the Materializer
          is for. */}
      <Place
        place="materializer"
        glyph={<Sparkles size={14} aria-hidden />}
        name="Materializer"
        count={materializedRail.total}
        countTitle={`${materializedRail.total} ${
          materializedRail.total === 1 ? "file" : "files"
        } made in the Materializer`}
        current={!searching && filter.place === "materializer" && filter.folderId === ""}
        expanded={expanded.materializer}
        onToggle={() => toggle("materializer")}
        onSelect={() => go("materializer", "")}
        emptyText="Nothing has been materialized yet."
        empty={materializedRail.folders.length === 0}
      >
        {materializedRail.folders.map((entry) => (
          <button
            key={entry.folder.id}
            type="button"
            className="os-files-node"
            data-current={
              !searching &&
              filter.place === "materializer" &&
              filter.folderId === entry.folder.id
                ? true
                : undefined
            }
            onClick={() => go("materializer", entry.folder.id)}
          >
            <Folder size={14} aria-hidden />
            <span className="os-files-node-name">{entry.folder.name}</span>
            <span className="os-files-node-count">{entry.count}</span>
          </button>
        ))}
      </Place>

      {railNote !== "" ? <Caption>{railNote}</Caption> : null}
      {foldersState === "degraded" || foldersState === "disconnected" ? (
        <Caption>Folder updates are behind -- showing the last known tree.</Caption>
      ) : null}
    </nav>
  );
}

/**
 * One place: a disclosure and a destination on the same line.
 *
 * TWO BUTTONS, NOT ONE WITH A NESTED HIT AREA. A button inside a button is
 * invalid HTML and behaves differently in every engine; two siblings in a
 * flex row give the chevron its own accessible name ("Expand Archive") and
 * its own `aria-expanded`, which is what a screen reader needs to say the
 * place is shut without also claiming the destination is.
 */
function Place({
  place,
  glyph,
  name,
  count,
  countTitle,
  current,
  expanded,
  onToggle,
  onSelect,
  empty,
  emptyText,
  groupClass,
  children,
}: {
  place: FilesPlace;
  glyph: ReactNode;
  name: string;
  count: number;
  countTitle: string;
  current: boolean;
  expanded: boolean;
  onToggle: () => void;
  onSelect: () => void;
  empty: boolean;
  emptyText: string;
  /** An extra class on the group, for a place whose contents nest. */
  groupClass?: string;
  children: ReactNode;
}) {
  const groupId = `os-files-place-${place}`;
  return (
    <>
      <div className="os-files-place-row">
        <button
          type="button"
          className="os-files-disclose"
          aria-expanded={expanded}
          aria-controls={groupId}
          aria-label={`${expanded ? "Collapse" : "Expand"} ${name}`}
          onClick={onToggle}
        >
          <ChevronRight size={12} aria-hidden />
        </button>
        <button
          type="button"
          className="os-files-node os-files-place"
          data-current={current ? true : undefined}
          onClick={onSelect}
        >
          {glyph}
          <span className="os-files-node-name">{name}</span>
          {/* SHUT, AND SOMEWHERE ELSE. The number answers "what is in here
              that I cannot see", so it is wrong in both other cases: OPEN,
              the folders carry their own counts and this is a second, larger
              total sitting among them; CURRENT, the Head already names the
              scope and counts the rows listed in it (DESIGN.md rule 7 -- the
              rail highlights the scope, the Head names it), which on first
              open put "Library 2" in the Head beside "Library 4" here. Two
              numbers under one word is worse than none. */}
          {!expanded && !current && count > 0 ? (
            <span className="os-files-node-count" title={countTitle}>
              {count}
            </span>
          ) : null}
        </button>
      </div>
      <div
        id={groupId}
        className={groupClass ? `os-files-place-group ${groupClass}` : "os-files-place-group"}
        hidden={!expanded}
      >
        {!empty ? children : emptyText !== "" ? (
          <p className="os-files-place-empty">{emptyText}</p>
        ) : null}
      </div>
    </>
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

/** The reserved disclosure key for the loose group. A folder id is a short id
 *  (hex), so it can never collide with this. */
const LOOSE_GROUP = " loose";

/**
 * What the shut Bin's number means, spelled out.
 *
 * The number itself is files PLUS folders, which is the count the Bin app
 * lists -- both are things a person put there and both are things they can
 * take back, so a Bin summarising as its file count alone would stand in
 * front of folders it does not mention. The title says which is which,
 * because "4" over a Bin holding one file and three folders is true and
 * unhelpful.
 */
function binSummary(bin: BinRail): string {
  const files = `${bin.fileCount} ${bin.fileCount === 1 ? "file" : "files"}`;
  const folders = `${bin.folderCount} ${bin.folderCount === 1 ? "folder" : "folders"}`;
  if (bin.folderCount === 0) return `${files} in the Bin`;
  if (bin.fileCount === 0) return `${folders} in the Bin`;
  return `${files} and ${folders} in the Bin`;
}

/**
 * One disclosure inside the Bin: an archived folder, or the loose group.
 *
 * ONE COMPONENT FOR BOTH, because they are the same affordance and a second
 * copy of it is a second set of aria wiring to keep in step. They differ in
 * exactly two ways, both passed in: the loose group is not a destination
 * (`onScope` null -- there is no folder to scope the list to) and it has no
 * menu (`onMenu` null -- nothing about a set can be restored).
 *
 * A FOLDER WITH NOTHING IN THE BIN KEEPS ITS ROW AND LOSES ITS CHEVRON. It is
 * still in the Bin and still restorable -- everything in it was restored
 * without it, or moved out -- but a twisty that opens onto one line of grey
 * text is the kind of affordance that teaches people not to trust twisties.
 * The gutter stays, so every name in the group still aligns.
 */
function BinGroup({
  id,
  glyph,
  name,
  files,
  emptyText,
  current,
  selectedFileId,
  expanded,
  onToggle,
  onScope,
  onMenu,
  onSelectFile,
}: {
  id: string;
  glyph: ReactNode;
  name: string;
  files: readonly ArtifactRow[];
  emptyText: string;
  current: boolean;
  selectedFileId: string;
  expanded: boolean;
  onToggle: () => void;
  onScope: (() => void) | null;
  onMenu: ((x: number, y: number) => void) | null;
  onSelectFile: (row: ArtifactRow) => void;
}) {
  const groupId = `os-files-bin-${id === LOOSE_GROUP ? "loose" : id}`;
  const openable = files.length > 0;
  const body = (
    <>
      {glyph}
      <span className="os-files-node-name">{name}</span>
      <span className="os-files-node-count">{files.length > 0 ? files.length : ""}</span>
    </>
  );
  return (
    <>
      <div className="os-files-bin-row">
        {openable ? (
          <button
            type="button"
            className="os-files-disclose"
            aria-expanded={expanded}
            aria-controls={groupId}
            aria-label={`${expanded ? "Collapse" : "Expand"} ${name}`}
            onClick={onToggle}
          >
            <ChevronRight size={12} aria-hidden />
          </button>
        ) : (
          <span className="os-files-rail-gutter" aria-hidden />
        )}
        {onScope === null ? (
          // A set is not a destination, so it is not a button. Nothing here is
          // clickable except the chevron, and that is the honest shape: "not
          // in a folder" cannot be opened, only looked inside.
          <span className="os-files-node os-files-node-static">{body}</span>
        ) : (
          <button
            type="button"
            className="os-files-node"
            data-current={current ? true : undefined}
            onClick={onScope}
            onContextMenu={(event) => {
              if (onMenu === null) return;
              // The rail offers its own menu, so the browser's stays out.
              event.preventDefault();
              event.stopPropagation();
              onMenu(event.clientX, event.clientY);
            }}
          >
            {body}
          </button>
        )}
      </div>
      {/* SHUT MEANS NOT RENDERED HERE, unlike a place, and the difference is
          volume. A place has folders under it and there are tens of those; a
          Bin group has FILES under it and there can be thousands, so
          `hidden` alone would put every archived file in the document for a
          rail nobody has opened. It also keeps a shut group out of every
          text query -- a hidden node is still found by `getByText`, so the
          rail would have been answering for rows it was not showing. */}
      <div id={groupId} className="os-files-place-group" hidden={!expanded}>
        {!expanded ? null : files.length === 0 ? (
          emptyText !== "" ? (
            <p className="os-files-place-empty">{emptyText}</p>
          ) : null
        ) : (
          files.map((row) => (
            <BinFile
              key={row.id}
              row={row}
              current={selectedFileId === row.id}
              onSelect={() => onSelectFile(row)}
            />
          ))
        )}
      </div>
    </>
  );
}

/**
 * One archived file in the rail.
 *
 * CLICKING IT SHOWS IT, which means two things at once: the list scopes to
 * where the file is and the row is selected, so the inspector opens on it.
 * Naming a thing and then leaving the person to find it in the list would be
 * the rail pointing at something it refuses to reach.
 */
function BinFile({
  row,
  current,
  onSelect,
}: {
  row: ArtifactRow;
  current: boolean;
  onSelect: () => void;
}) {
  return (
    <button
      type="button"
      className="os-files-node os-files-bin-file"
      data-current={current ? true : undefined}
      // Two levels of indent in a 184px column truncates sooner than the
      // list does, so the whole name is one hover away. The button has text,
      // so this does not become its accessible name.
      title={artifactName(row)}
      onClick={onSelect}
    >
      {kindGlyph(row.kind, 14)}
      <span className="os-files-node-name">{artifactName(row)}</span>
    </button>
  );
}
