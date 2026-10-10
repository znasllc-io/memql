import { useState } from "react";
import { Folder, MoreHorizontal } from "lucide-react";
import { IconButton } from "../../kit/IconButton";
import { RecordList, RecordRow, RecordListSkeleton } from "../../kit";
import type { FolderRow } from "./rows";

export function FileFolders({ folders, counts, loading, renamingId, onOpen, onRename, onCancelRename, onMenu }: {
  folders: readonly FolderRow[]; counts: ReadonlyMap<string, number>; loading: boolean;
  renamingId: string; onOpen: (id: string) => void; onRename: (id: string, name: string) => void;
  onCancelRename: () => void; onMenu: (x: number, y: number, folder: FolderRow) => void;
}) {
  if (loading && folders.length === 0) return <RecordListSkeleton label="Loading folders" />;
  if (folders.length === 0) return null;
  return <RecordList label="Folders" as="ul" className="os-file-folders">
    {folders.map(folder => <div key={folder.id} onContextMenu={event => { event.preventDefault(); onMenu(event.clientX, event.clientY, folder); }}>
      {renamingId === folder.id ? <FolderRename key={folder.id} folder={folder} onRename={onRename} onCancel={onCancelRename} /> :
        <RecordRow icon={<Folder size={17} aria-hidden />} name={folder.name} label={`Open folder ${folder.name}`}
          secondary={`${counts.get(folder.id) ?? 0} ${(counts.get(folder.id) ?? 0) === 1 ? "file" : "files"}`} onOpen={() => onOpen(folder.id)}
          actionLayout="compact" actions={<IconButton label={`Actions for folder ${folder.name}`} onClick={event => { const rect = event.currentTarget.getBoundingClientRect(); onMenu(rect.right, rect.bottom, folder); }}><MoreHorizontal size={16} aria-hidden /></IconButton>} />}
    </div>)}
  </RecordList>;
}
function FolderRename({ folder, onRename, onCancel }: { folder: FolderRow; onRename: (id: string, name: string) => void; onCancel: () => void }) {
  const [name, setName] = useState(folder.name);
  return <form className="os-files-rename" onSubmit={event => { event.preventDefault(); if (name.trim()) onRename(folder.id, name.trim()); }}>
    <input autoFocus aria-label={`Rename ${folder.name}`} value={name} onChange={event => setName(event.target.value)} onKeyDown={event => { if (event.key === "Escape") onCancel(); }} />
    <button type="submit" className="os-link" disabled={!name.trim()}>Save</button><button type="button" className="os-link" onClick={onCancel}>Cancel</button>
  </form>;
}
