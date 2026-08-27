import { useEffect, useRef, useState } from "react";
import { Folder as FolderGlyph, FolderOpen } from "lucide-react";

// A desktop folder (spec D4): opens as a popover grid anchored to the
// icon -- never a window. Inline rename; drag files in/out is handled by
// the desktop's dnd layer, which marks the icon as a drop target.

export interface FolderEntryView {
  id: string;
  title: string;
}

export function FolderIcon({
  id,
  name,
  count,
  open,
  isDropTarget,
  onToggle,
  onRename,
}: {
  id: string;
  name: string;
  count: number;
  open: boolean;
  isDropTarget: boolean;
  onToggle: () => void;
  onRename: (name: string) => void;
}) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(name);
  const inputRef = useRef<HTMLInputElement | null>(null);

  useEffect(() => {
    if (editing) inputRef.current?.select();
  }, [editing]);
  useEffect(() => setDraft(name), [name]);

  const Glyph = open ? FolderOpen : FolderGlyph;

  return (
    <div className="os-folder" data-os-folder={id} data-drop-target={isDropTarget || undefined}>
      <button
        type="button"
        className="os-file-button"
        aria-label={`${name}, folder, ${count} ${count === 1 ? "file" : "files"}`}
        aria-expanded={open}
        onClick={onToggle}
      >
        <span className="os-file-glyph">
          <Glyph size={26} aria-hidden />
        </span>
        {editing ? (
          <input
            ref={inputRef}
            className="os-folder-rename"
            value={draft}
            aria-label="Folder name"
            onClick={(e) => e.stopPropagation()}
            onChange={(e) => setDraft(e.target.value)}
            onBlur={() => {
              setEditing(false);
              if (draft.trim() && draft !== name) onRename(draft.trim());
            }}
            onKeyDown={(e) => {
              e.stopPropagation();
              if (e.key === "Enter") (e.target as HTMLInputElement).blur();
              if (e.key === "Escape") {
                setDraft(name);
                setEditing(false);
              }
            }}
          />
        ) : (
          <span
            className="os-file-title"
            onDoubleClick={(e) => {
              e.stopPropagation();
              setEditing(true);
            }}
          >
            {name}
          </span>
        )}
      </button>
    </div>
  );
}
