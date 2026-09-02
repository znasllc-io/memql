import { type ReactNode } from "react";
import {
  Archive as ArchiveGlyph,
  ChevronRight,
  Folder,
  HardDrive,
  Monitor,
} from "lucide-react";
import type { LiveState } from "@znasllc-io/memql-sdk-core/client";

import { Caption, Chip } from "../../kit";
import type { FilesFilter, FilesPlace } from "./filters";
import type { FolderTree, TreeNode } from "./fold";
import { LINK_LABEL, LINK_SENTENCE, type LinkState } from "./links";
import type { FolderRow } from "./rows";
import type { DeskFolderShortcut } from "./BrowseSection";

// The rail: three PLACES, each a disclosure over its own folders.
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
// "Archive" is the gesture for "show me what is in Archive", so answering it
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
// THE RAIL LISTS FOLDERS, NOT FILES. Files are what the list beside it is
// for, and a second copy of them in a 200px column would be the same rows
// twice, narrower. Opening a place scopes the list; that is where its files
// appear.

/** Which places are open. Held by the app root so switching to Settings and
 *  back does not shut everything the person just opened. */
export type ExpandedPlaces = Record<FilesPlace, boolean>;

export const ALL_COLLAPSED: ExpandedPlaces = {
  library: false,
  desktop: false,
  archive: false,
};

export function Rail({
  filter,
  patch,
  tree,
  counts,
  archivedCounts,
  folderLinks,
  archivedFolders,
  deskFolders,
  folderNameOf,
  libraryTotal,
  deskFileCount,
  archivedTotal,
  expanded,
  setExpanded,
  renamingFolderId,
  onRename,
  onCancelRename,
  onFolderMenu,
  onArchivedFolderMenu,
  railNote,
  foldersState,
}: {
  filter: FilesFilter;
  patch: (next: Partial<FilesFilter>) => void;
  tree: FolderTree;
  counts: Map<string, number>;
  archivedCounts: Map<string, number>;
  folderLinks: Map<string, LinkState>;
  archivedFolders: FolderRow[];
  deskFolders: DeskFolderShortcut[];
  folderNameOf: (folderId: string) => string;
  /** Everything live in the Library -- the collapsed summary's honest number. */
  libraryTotal: number;
  deskFileCount: number;
  archivedTotal: number;
  expanded: ExpandedPlaces;
  setExpanded: (next: ExpandedPlaces) => void;
  renamingFolderId: string;
  onRename: (folderId: string, name: string) => void;
  onCancelRename: () => void;
  onFolderMenu: (x: number, y: number, node: TreeNode) => void;
  onArchivedFolderMenu: (x: number, y: number, folder: FolderRow) => void;
  railNote: string;
  foldersState: LiveState;
}) {
  const searching = filter.search.trim() !== "";
  const open = (place: FilesPlace) => setExpanded({ ...expanded, [place]: true });
  const toggle = (place: FilesPlace) =>
    setExpanded({ ...expanded, [place]: !expanded[place] });
  const go = (place: FilesPlace, folderId: string) => {
    patch({ place, folderId, search: "" });
    open(place);
  };

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

      <Place
        place="archive"
        glyph={<ArchiveGlyph size={14} aria-hidden />}
        name="Archive"
        count={archivedTotal}
        countTitle={`${archivedTotal} archived`}
        current={!searching && filter.place === "archive" && filter.folderId === ""}
        expanded={expanded.archive}
        onToggle={() => toggle("archive")}
        onSelect={() => go("archive", "")}
        emptyText="Nothing archived."
        empty={archivedFolders.length === 0}
      >
        {archivedFolders.map((folder) => (
          <button
            key={folder.id}
            type="button"
            className="os-files-node"
            data-current={
              !searching && filter.place === "archive" && filter.folderId === folder.id
                ? true
                : undefined
            }
            onClick={() => go("archive", folder.id)}
            onContextMenu={(event) => {
              event.preventDefault();
              event.stopPropagation();
              onArchivedFolderMenu(event.clientX, event.clientY, folder);
            }}
          >
            <Folder size={14} aria-hidden />
            <span className="os-files-node-name">{folder.name}</span>
            <span className="os-files-node-count">
              {(archivedCounts.get(folder.id) ?? 0) > 0 ? archivedCounts.get(folder.id) : ""}
            </span>
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
      <div id={groupId} className="os-files-place-group" hidden={!expanded}>
        {empty ? <p className="os-files-place-empty">{emptyText}</p> : children}
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
