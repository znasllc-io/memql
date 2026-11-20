import { useEffect, useState, type CSSProperties, type ReactElement } from "react";
import { ChevronDown, ChevronRight, Folder, Inbox, Layers, Library, Monitor, Send, Trash2, Users } from "lucide-react";

import type { FolderTree, TreeNode } from "./fold";
import type { FilesPlace } from "./filters";

// The Files rail (the "Places" redesign the owner picked, Nov 2026): the
// Library's folder tree first, then the other places. It replaces the row of
// location tabs above the list, so a folder three levels down is one click
// away instead of three.
//
// NOUNS ONLY (DESIGN.md rule 6): places and folders, nothing you do. Adding a
// folder stays on the Head's one Add control, and a folder's verbs stay on its
// right-click menu. It carries no counts either (rule 7, say it once): the
// Head names the scope and counts what is listed.

type Place = { place: Exclude<FilesPlace, "library">; label: string; icon: ReactElement };

const PLACES_BEFORE: readonly Place[] = [
  { place: "desktop", label: "Desktop", icon: <Monitor size={15} aria-hidden /> },
  { place: "materializer", label: "Materializer", icon: <Layers size={15} aria-hidden /> },
];

// "SHARED" OPENS AND CLOSES LIKE A FOLDER (Jesús, Nov 19 2026, draft A; José:
// not "Shared with me" but just "Shared", "because you can also share
// yourself"). Its two entries are places of their own: what other people sent
// you, and what you are sending. "Shared" itself is not a place -- there is no
// page that is both -- so pressing its name opens or shuts it, like its arrow.
const SHARED: readonly Place[] = [
  { place: "shared", label: "Shared with me", icon: <Inbox size={15} aria-hidden /> },
  { place: "sharing", label: "Sharing", icon: <Send size={15} aria-hidden /> },
];

const PLACES_AFTER: readonly Place[] = [
  { place: "bin", label: "Bin", icon: <Trash2 size={15} aria-hidden /> },
];

/** The disclosure key for "Shared"; no folder id is ever this. */
const SHARED_BRANCH = "~shared";

/** The folder ids from a root down to `folderId`, bounded against a cycle in the data. */
function ancestry(tree: FolderTree, folderId: string): string[] {
  const chain: string[] = [];
  const seen = new Set<string>();
  for (let id = folderId; id !== "" && !seen.has(id) && chain.length < 32; ) {
    seen.add(id);
    chain.push(id);
    id = tree.byId.get(id)?.folder.parentFolderId ?? "";
  }
  return chain;
}

export function FilesRail({
  tree,
  place,
  folderId,
  onPlace,
  onFolder,
  onFolderMenu,
}: {
  tree: FolderTree;
  place: FilesPlace;
  /** The folder being looked at, "" at a place's root. */
  folderId: string;
  onPlace: (place: FilesPlace) => void;
  onFolder: (folderId: string) => void;
  onFolderMenu: (x: number, y: number, node: TreeNode) => void;
}) {
  // Which branches are open. The way down to the folder being looked at, and
  // that folder itself, are opened on arrival, so arriving somewhere by the
  // trail, a tile or the desk never leaves it hidden inside a shut branch, and
  // what it holds shows beneath it. A branch shut by hand stays shut.
  // "Shared" starts open, like the Library: its two entries were one click
  // away before it became a folder, and a person who shuts it keeps it shut.
  const [open, setOpen] = useState<ReadonlySet<string>>(() => new Set(["library", SHARED_BRANCH]));
  const libraryFolder = place === "library" ? folderId : "";
  // Arriving in one of Shared's places by any other way (an intent, a link)
  // opens it, as arriving in a folder opens the way down to it.
  const inShared = SHARED.some((entry) => entry.place === place);
  useEffect(() => {
    if (inShared) setOpen((prev) => (prev.has(SHARED_BRANCH) ? prev : new Set([...prev, SHARED_BRANCH])));
  }, [inShared]);
  useEffect(() => {
    if (libraryFolder === "") return;
    const chain = ancestry(tree, libraryFolder);
    setOpen((prev) => (chain.every((id) => prev.has(id)) && prev.has("library") ? prev : new Set([...prev, "library", ...chain])));
  }, [tree, libraryFolder]);
  const toggle = (id: string) =>
    setOpen((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  const placeRow = (entry: Place, depth = 0): ReactElement => (
    <li key={entry.place}>
      <RailRow
        depth={depth}
        label={entry.label}
        icon={entry.icon}
        current={place === entry.place}
        onSelect={() => onPlace(entry.place)}
      />
    </li>
  );

  const branch = (nodes: readonly TreeNode[], depth: number): ReactElement => (
    <ul className="os-files-rail-list">
      {nodes.map((node) => {
        const id = node.folder.id;
        const expanded = open.has(id);
        return (
          <li key={id}>
            <RailRow
              depth={depth}
              label={node.folder.name}
              icon={<Folder size={15} aria-hidden />}
              current={place === "library" && folderId === id}
              expanded={node.children.length > 0 ? expanded : undefined}
              onToggle={() => toggle(id)}
              onSelect={() => onFolder(id)}
              onMenu={(x, y) => onFolderMenu(x, y, node)}
            />
            {node.children.length > 0 && expanded ? branch(node.children, depth + 1) : null}
          </li>
        );
      })}
    </ul>
  );

  return (
    <nav className="os-files-rail" aria-label="File locations">
      <ul className="os-files-rail-list">
        <li>
          <RailRow
            depth={0}
            label="Library"
            icon={<Library size={15} aria-hidden />}
            current={place === "library" && folderId === ""}
            expanded={tree.roots.length > 0 ? open.has("library") : undefined}
            onToggle={() => toggle("library")}
            onSelect={() => onPlace("library")}
          />
          {tree.roots.length > 0 && open.has("library") ? branch(tree.roots, 1) : null}
        </li>
      </ul>
      <ul className="os-files-rail-list">
        {PLACES_BEFORE.map(placeRow)}
        <li>
          <RailRow
            depth={0}
            label="Shared"
            icon={<Users size={15} aria-hidden />}
            current={false}
            expanded={open.has(SHARED_BRANCH)}
            onToggle={() => toggle(SHARED_BRANCH)}
            onSelect={() => toggle(SHARED_BRANCH)}
            toggleLabel={open.has(SHARED_BRANCH) ? "Hide Shared with me and Sharing" : "Show Shared with me and Sharing"}
          />
          {open.has(SHARED_BRANCH) ? <ul className="os-files-rail-list">{SHARED.map((entry) => placeRow(entry, 1))}</ul> : null}
        </li>
        {PLACES_AFTER.map((entry) => placeRow(entry))}
      </ul>
    </nav>
  );
}

function RailRow({
  depth,
  label,
  icon,
  current,
  expanded,
  onToggle,
  onSelect,
  onMenu,
  toggleLabel,
}: {
  depth: number;
  label: string;
  icon: ReactElement;
  current: boolean;
  /** undefined: nothing inside to show, so no disclosure at all. */
  expanded?: boolean;
  onToggle?: () => void;
  onSelect: () => void;
  onMenu?: (x: number, y: number) => void;
  /** What the disclosure says it does; a folder's own sentence by default. */
  toggleLabel?: string;
}) {
  return (
    <div
      className="os-files-rail-row"
      data-current={current || undefined}
      style={{ "--depth": depth } as CSSProperties}
      onContextMenu={
        onMenu
          ? (event) => {
              // The shell's right-click rule: a surface with its own menu
              // says so, and stops the root handler re-enabling the browser's.
              event.preventDefault();
              event.stopPropagation();
              onMenu(event.clientX, event.clientY);
            }
          : undefined
      }
    >
      {expanded === undefined ? (
        <span className="os-files-rail-twist" aria-hidden />
      ) : (
        <button
          type="button"
          className="os-files-rail-twist"
          aria-expanded={expanded}
          aria-label={toggleLabel ?? `${expanded ? "Hide" : "Show"} folders in ${label}`}
          onClick={onToggle}
        >
          {expanded ? <ChevronDown size={12} aria-hidden /> : <ChevronRight size={12} aria-hidden />}
        </button>
      )}
      <button
        type="button"
        className="os-files-rail-place"
        aria-current={current ? "page" : undefined}
        onClick={onSelect}
        onKeyDown={
          onMenu
            ? (event) => {
                if (event.key === "ContextMenu" || (event.key === "F10" && event.shiftKey)) {
                  event.preventDefault();
                  const rect = event.currentTarget.getBoundingClientRect();
                  onMenu(rect.left + 12, rect.bottom);
                }
              }
            : undefined
        }
      >
        {icon}
        <span className="os-files-rail-label">{label}</span>
      </button>
    </div>
  );
}
