import { useState } from "react";
import { RecordListSkeleton } from "../../kit";
import { FolderGlyph, useLongPress } from "./Tiles";
import type { FolderRow } from "./rows";

// The folders of the place being looked at, as tiles at the head of the grid.
// A folder OPENS on a single click (it is a place to go, and the rail's own
// rows open the same way); a file only selects (see FileTile).
export function FileFolders({ folders, counts, loading, renamingId, onOpen, onRename, onCancelRename, onMenu }: {
  folders: readonly FolderRow[]; counts: ReadonlyMap<string, number> | null; loading: boolean;
  renamingId: string; onOpen: (id: string) => void; onRename: (id: string, name: string) => void;
  onCancelRename: () => void; onMenu: (x: number, y: number, folder: FolderRow) => void;
}) {
  if (loading && folders.length === 0) return <RecordListSkeleton label="Loading folders" />;
  if (folders.length === 0) return null;
  return <ul className="os-files-grid" aria-label="Folders">
    {folders.map(folder => {
      const count = counts ? (counts.get(folder.id) ?? 0) : undefined;
      return <li key={folder.id} onContextMenu={event => { event.preventDefault(); event.stopPropagation(); onMenu(event.clientX, event.clientY, folder); }}>
        {renamingId === folder.id ? <FolderRename key={folder.id} folder={folder} onRename={onRename} onCancel={onCancelRename} /> :
          <FolderTile folder={folder} count={count} onOpen={onOpen} onMenu={onMenu} />}
      </li>;
    })}
  </ul>;
}
function FolderTile({ folder, count, onOpen, onMenu }: {
  folder: FolderRow; count: number | undefined; onOpen: (id: string) => void; onMenu: (x: number, y: number, folder: FolderRow) => void;
}) {
  const press = useLongPress((x, y) => onMenu(x, y, folder));
  return <button type="button" className="os-files-tile" data-archived={folder.archived || undefined} aria-label={`Open folder ${folder.name}`}
    onPointerDown={press.onPointerDown} onPointerUp={press.onPointerUp} onPointerCancel={press.onPointerCancel} onPointerLeave={press.onPointerLeave}
    onClick={() => { if (!press.consumed()) onOpen(folder.id); }}
    onKeyDown={event => { if (event.key === "ContextMenu" || (event.key === "F10" && event.shiftKey)) { event.preventDefault(); const rect = event.currentTarget.getBoundingClientRect(); onMenu(rect.left + 12, rect.bottom, folder); } }}>
    <span className="os-files-tile-icon"><FolderGlyph /></span>
    <span className="os-files-tile-name">{folder.name}</span>
    {count === undefined ? null : <span className="os-files-tile-meta">{count} {count === 1 ? "file" : "files"}</span>}
  </button>;
}
function FolderRename({ folder, onRename, onCancel }: { folder: FolderRow; onRename: (id: string, name: string) => void; onCancel: () => void }) {
  const [name, setName] = useState(folder.name);
  // In the tile's own shape, so renaming does not reflow the grid around it.
  return <form className="os-files-rename os-files-tile-rename" onSubmit={event => { event.preventDefault(); if (name.trim()) onRename(folder.id, name.trim()); }}>
    <span className="os-files-tile-icon"><FolderGlyph /></span>
    <input autoFocus aria-label={`Rename ${folder.name}`} value={name} onChange={event => setName(event.target.value)} onKeyDown={event => { if (event.key === "Escape") onCancel(); }} />
    <span className="os-files-rename-acts"><button type="submit" className="os-link" disabled={!name.trim()}>Save</button><button type="button" className="os-link" onClick={onCancel}>Cancel</button></span>
  </form>;
}
