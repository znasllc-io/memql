import { useRef, type PointerEvent as ReactPointerEvent } from "react";
import { useDraggable } from "@dnd-kit/core";

import type { DeskFileDrag } from "../../chrome/Desktop";
import { artifactName, deskEntryOf, type ArtifactRow } from "./rows";
import { useOneAtATime } from "../../kit/oneAtATime";

// The grid's glyphs: one thin outline each, the way Fleet draws a machine, with
// a file's type written inside it in the data face rather than a coloured icon
// per format. Drawn in currentColor so the stylesheet decides the stroke.

/**
 * A held finger opens a tile's menu. Safari on iPhone and iPad sends no
 * contextmenu on a long press, and a click on a file only selects it now, so
 * without this a file's verbs would be out of reach on a touch screen. Mouse
 * and pen are left to their own right-click.
 */
export function useLongPress(onMenu: (x: number, y: number) => void) {
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const fired = useRef(false);
  const stop = () => {
    if (timer.current !== null) clearTimeout(timer.current);
    timer.current = null;
  };
  return {
    onPointerDown: (event: ReactPointerEvent) => {
      fired.current = false;
      if (event.pointerType !== "touch") return;
      const { clientX, clientY } = event;
      stop();
      timer.current = setTimeout(() => {
        fired.current = true;
        onMenu(clientX, clientY);
      }, 500);
    },
    onPointerUp: stop,
    onPointerCancel: stop,
    onPointerLeave: stop,
    /** True once, for the click that ends a press which opened the menu. */
    consumed: () => {
      const was = fired.current;
      fired.current = false;
      return was;
    },
  };
}

export function FolderGlyph() {
  return (
    <svg className="os-files-glyph os-files-glyph-folder" viewBox="0 0 62 50" aria-hidden="true">
      <path d="M56.5 49.4a4.9 4.9 0 0 0 4.9-4.9V15.6a4.9 4.9 0 0 0-4.9-4.9H30.6a4.9 4.9 0 0 1-4.1-2.2L23.9 4.8A4.9 4.9 0 0 0 19.8 2.6H5.5A4.9 4.9 0 0 0 .6 7.5v37a4.9 4.9 0 0 0 4.9 4.9Z" />
    </svg>
  );
}

/**
 * What a file's glyph says it is: the name's extension, else the recorded
 * format, at most five characters (TAR.GZ reads as GZ). Empty when neither
 * says anything, and the outline then stands alone. A Library tile and its
 * desk icon both ask this, so the two always say the same thing.
 */
export function typeLabel(name: string, format = ""): string {
  const dot = name.lastIndexOf(".");
  const ext = dot > 0 && dot < name.length - 1 ? name.slice(dot + 1) : format;
  return /^[a-z0-9]{1,5}$/i.test(ext) ? ext.toUpperCase() : "";
}

export function FileGlyph({ label }: { label: string }) {
  return (
    <span className="os-files-sheet">
      <svg className="os-files-glyph" viewBox="0 0 50 62" aria-hidden="true">
        <path d="M4 .6h30L49.4 16V58a3.4 3.4 0 0 1-3.4 3.4H4A3.4 3.4 0 0 1 .6 58V4A3.4 3.4 0 0 1 4 .6Z" />
        <path d="M34 .6V12.6a3.4 3.4 0 0 0 3.4 3.4h12" fill="none" />
      </svg>
      {label ? <b aria-hidden="true">{label}</b> : null}
    </span>
  );
}

/**
 * One file in the grid. A click SELECTS it and nothing else opens (the owner,
 * Nov 2026: the details panel should not pop up on a click); double-click or
 * Enter opens the file; right-click, the ContextMenu key or Shift+F10 opens
 * its menu, which is where its other verbs live.
 */
export function FileTile({
  row,
  picked,
  dim,
  hint,
  tick,
  onPick,
  onOpen,
  onMenu,
  caption = "",
  fixed = false,
}: {
  row: ArtifactRow;
  picked: boolean;
  dim: boolean;
  /** Where the file came from, as a tooltip: the story the list row used to print. */
  hint: string;
  /** The live list's arrival cue: a file that just landed says so. */
  tick: "added" | "updated" | null;
  onPick: () => void;
  /**
   * Double-click or Enter. An open that returns its promise (a download: a
   * shared file, a zip) is held like a kit button (kit/oneAtATime), so a burst
   * of double-clicks downloads once and the next only after half a second
   * (Jesús, Nov 20 2026).
   */
  onOpen: () => unknown;
  onMenu: (x: number, y: number) => void;
  /** A quiet line under the name (who shared a file with you). */
  caption?: string;
  /** Not draggable (to the Bin or the desk): a file shared with you is not yours. */
  fixed?: boolean;
}) {
  // DRAGGABLE TO THE BIN (memql#4784) AND ONTO THE DESK (José, Nov 19 2026).
  // The payload travels with the drag, because neither the dock nor the desk
  // holds a Library feed of its own. The listeners go on a wrapper and
  // dnd-kit's `attributes` are not spread: they would make the wrapper a
  // second focusable control announcing the same file.
  const draggable = useDraggable({
    id: `artifact:${row.id}`,
    data: {
      artifactId: row.id,
      name: artifactName(row),
      folder: false,
      deskItemId: "",
      desk: { entry: deskEntryOf(row), glyph: typeLabel(artifactName(row), row.format) },
    } satisfies DeskFileDrag,
    disabled: fixed,
  });
  const name = artifactName(row);
  const guard = useOneAtATime();
  const open = () => guard.press(onOpen, false);
  const press = useLongPress((x, y) => {
    onPick();
    onMenu(x, y);
  });
  return (
    <div
      ref={draggable.setNodeRef}
      className="os-files-tile-wrap"
      data-dragging={draggable.isDragging || undefined}
      onContextMenu={(event) => {
        // The shell's right-click rule: a surface with its own menu says so,
        // and stops the root handler from re-enabling the browser's.
        event.preventDefault();
        event.stopPropagation();
        onPick();
        onMenu(event.clientX, event.clientY);
      }}
      {...draggable.listeners}
    >
      <button
        type="button"
        className="os-files-tile"
        aria-pressed={picked}
        data-archived={dim || undefined}
        title={hint ? `${name}\n${hint}` : name}
        onPointerDown={press.onPointerDown}
        onPointerUp={press.onPointerUp}
        onPointerCancel={press.onPointerCancel}
        onPointerLeave={press.onPointerLeave}
        onClick={() => {
          if (!press.consumed()) onPick();
        }}
        onDoubleClick={open}
        onKeyDown={(event) => {
          if (event.key === "Enter") {
            event.preventDefault();
            open();
          } else if (event.key === "ContextMenu" || (event.key === "F10" && event.shiftKey)) {
            event.preventDefault();
            onPick();
            const rect = event.currentTarget.getBoundingClientRect();
            onMenu(rect.left + 12, rect.bottom);
          }
        }}
      >
        <span className="os-files-tile-icon">
          <FileGlyph label={typeLabel(name, row.format)} />
        </span>
        <span className="os-files-tile-name">{name}</span>
        {caption ? <span className="os-files-tile-meta os-files-tile-from">{caption}</span> : null}
        {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
      </button>
    </div>
  );
}
