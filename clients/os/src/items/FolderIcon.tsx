import { useEffect, useRef, useState } from "react";
import { Folder as FolderGlyph } from "lucide-react";

// A desktop folder (epic memql#4842, #4847): a real desktop icon. Single
// click SELECTS -- exactly as a file icon does -- and double-click (or
// Enter) opens the Files app scoped to this folder under the Desktop place.
// The under-icon popover this replaces is gone, and with it the open/closed
// glyph: a desk icon is a shortcut, not a disclosure.
//
// Rename is driven from the item's context menu (the desktop owns the
// `renaming` state); the old rename-on-title-double-click gesture collided
// with open and went with the popover.

export function FolderIcon({
  id,
  name,
  selected,
  isDropTarget,
  renaming,
  onSelect,
  onOpen,
  onRenameCommit,
  onRenameCancel,
}: {
  id: string;
  name: string;
  selected: boolean;
  isDropTarget: boolean;
  renaming: boolean;
  onSelect: () => void;
  onOpen: () => void;
  onRenameCommit: (name: string) => void;
  onRenameCancel: () => void;
}) {
  const [draft, setDraft] = useState(name);
  const inputRef = useRef<HTMLInputElement | null>(null);

  useEffect(() => {
    if (renaming) {
      setDraft(name);
      inputRef.current?.select();
    }
  }, [renaming, name]);

  return (
    <div
      className="os-folder"
      data-os-folder={id}
      data-selected={selected || undefined}
      data-drop-target={isDropTarget || undefined}
    >
      <button
        type="button"
        className="os-file-button"
        // No count in the name: a desk folder is a shortcut to a Library
        // folder, and a closed icon honestly does not know what is inside.
        aria-label={`${name}, folder -- opens in Files`}
        onClick={onSelect}
        onDoubleClick={onOpen}
        onKeyDown={(event) => {
          if (event.key === "Enter") onOpen();
        }}
      >
        <span className="os-file-glyph">
          <FolderGlyph size={26} aria-hidden />
        </span>
        {renaming ? (
          <input
            ref={inputRef}
            className="os-folder-rename"
            value={draft}
            aria-label="Folder name"
            autoFocus
            onClick={(e) => e.stopPropagation()}
            onDoubleClick={(e) => e.stopPropagation()}
            onChange={(e) => setDraft(e.target.value)}
            onBlur={() => {
              const next = draft.trim();
              if (next && next !== name) onRenameCommit(next);
              else onRenameCancel();
            }}
            onKeyDown={(e) => {
              e.stopPropagation();
              if (e.key === "Enter") (e.target as HTMLInputElement).blur();
              if (e.key === "Escape") {
                setDraft(name);
                onRenameCancel();
              }
            }}
          />
        ) : (
          <span className="os-file-title">{name}</span>
        )}
      </button>
    </div>
  );
}
