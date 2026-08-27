import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  DndContext,
  PointerSensor,
  pointerWithin,
  useDraggable,
  useDroppable,
  useSensor,
  useSensors,
  type DragEndEvent,
  type DragStartEvent,
} from "@dnd-kit/core";

import { browserHandoffPorts, openInVsCode, VSCODE_NO_ANSWER_MESSAGE, type HandoffPorts } from "../items/vscode";
import { FileIcon } from "../items/FileIcon";
import { FolderIcon } from "../items/FolderIcon";
import type { UploadProvider } from "../items/upload";
import { widgetById } from "../system/registry";
import { placeWindows, type PlacementTokens } from "../system/placement";
import type { DeskSurface, DesktopItem, FileEntry, GridPos } from "../system/desktop";
import type { Desk } from "../system/desks";
import { DeskNumeral, MemoryField } from "../wallpaper/MemoryField";
import { WidgetFrame } from "../widgets/WidgetFrame";
import { useMachines } from "../live/machines";
import { useSession } from "./access";
import { ContextMenu, type MenuEntry } from "./ContextMenu";
import { DeskPager } from "./DeskPager";
import { useOs } from "./state";
import { WindowFrame } from "./WindowFrame";

// The desktop (spec A/B): desk plates sliding horizontally, each carrying
// its surface (files, folders, widgets on the snap grid) and its windows
// at computed placements. Owns every drag (icons, folder in/out, window
// swap/throw), the desk + item context menus, host-file drop -> upload,
// and the VS Code handoff with its no-answer fallback.

export const CELL_W = 96;
export const CELL_H = 104;
const SURFACE_PAD = 20;

function cellToPx(pos: GridPos): { left: number; top: number } {
  return { left: SURFACE_PAD + pos.col * CELL_W, top: SURFACE_PAD + pos.row * CELL_H };
}

interface DeskMenu {
  x: number;
  y: number;
  kind: "desk" | "item";
  cell: GridPos;
  itemId?: string;
}

export function Desktop({
  viewport,
  placement,
  uploads,
  handoffPorts = browserHandoffPorts,
}: {
  viewport: { w: number; h: number };
  placement: PlacementTokens;
  uploads: UploadProvider;
  handoffPorts?: HandoffPorts;
}) {
  const { state, actions, registry, actorRole, grid } = useOs();
  const { config } = useSession();
  const [menu, setMenu] = useState<DeskMenu | null>(null);
  const [openFolderId, setOpenFolderId] = useState<string | null>(null);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [draggingWindow, setDraggingWindow] = useState(false);
  const [noAnswerFor, setNoAnswerFor] = useState<string | null>(null);
  const pendingFiles = useRef(new Map<string, File>());
  const cancelHandoff = useRef<(() => void) | null>(null);

  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 6 } }));
  const desks = state.shell.desks;
  const activeIndex = Math.max(0, desks.findIndex((d) => d.id === state.shell.activeDeskId));

  useEffect(() => () => cancelHandoff.current?.(), []);

  // ---- opening files: the VS Code handoff (spec D3) ----
  const openFile = useCallback(
    (item: Extract<DesktopItem, { kind: "file" }>) => {
      if (!item.artifactId || item.uploadState) return;
      cancelHandoff.current?.();
      setNoAnswerFor(null);
      cancelHandoff.current = openInVsCode(
        config.domain,
        item.artifactId,
        () => setNoAnswerFor(item.id),
        handoffPorts,
      );
    },
    [config.domain, handoffPorts],
  );

  useEffect(() => {
    if (!noAnswerFor) return;
    const t = setTimeout(() => setNoAnswerFor(null), 8000);
    return () => clearTimeout(t);
  }, [noAnswerFor]);

  // ---- host-file drop -> upload provider (spec B) ----
  const startUpload = useCallback(
    (file: File, preferred: GridPos, existingItemId?: string) => {
      const itemId = existingItemId ?? `up-${Date.now()}-${file.name}`;
      if (!existingItemId) {
        const placed = actions.addFile(
          {
            id: itemId,
            artifactId: "",
            title: file.name,
            fileKind: "file",
            source: "uploaded",
            uploadState: "uploading",
          },
          preferred,
        );
        if (!placed) return;
      } else {
        actions.updateFileItem(itemId, { uploadState: "uploading" });
      }
      pendingFiles.current.set(itemId, file);
      uploads
        .upload(file)
        .done.then((result) => {
          pendingFiles.current.delete(itemId);
          actions.updateFileItem(itemId, {
            artifactId: result.artifactId,
            title: result.title,
            fileKind: result.fileKind,
            source: result.source,
            uploadState: undefined,
          });
        })
        .catch(() => {
          actions.updateFileItem(itemId, { uploadState: "failed" });
        });
    },
    [actions, uploads],
  );

  const onHostDrop = useCallback(
    (event: React.DragEvent) => {
      if (!event.dataTransfer.files.length) return;
      event.preventDefault();
      const cell = pxToCell(event, grid);
      for (const file of Array.from(event.dataTransfer.files)) startUpload(file, cell);
    },
    [grid, startUpload],
  );

  // ---- shell drags: icons, folders, window swap/throw ----
  function onDragStart(event: DragStartEvent) {
    setDraggingWindow(String(event.active.id).startsWith("window:"));
  }

  function onDragEnd(event: DragEndEvent) {
    setDraggingWindow(false);
    const id = String(event.active.id);
    const overId = event.over ? String(event.over.id) : null;

    if (id.startsWith("window:")) {
      const windowId = id.slice("window:".length);
      if (overId?.startsWith("pagerdot:")) {
        const target = overId.slice("pagerdot:".length);
        actions.throwToDesk(windowId, target === "new" ? "new" : target);
        return;
      }
      // Two windows on the desk: dropping past the center line swaps.
      const desk = desks.find((d) => d.windows.includes(windowId));
      if (desk && desk.windows.length === 2) {
        const pointerX = pointerXOf(event);
        if (pointerX !== null) {
          const side = pointerX < viewport.w / 2 ? 0 : 1;
          if (desk.windows[side] !== windowId) actions.swapSides(desk.id);
        }
      }
      return;
    }

    if (id.startsWith("folderfile:")) {
      const [, folderId, fileId] = id.split(":");
      if (folderId && fileId && (!overId || overId === "surface" || overId.startsWith("plate:"))) {
        actions.takeFileOutOfFolder(folderId, fileId);
        setOpenFolderId(null);
      }
      return;
    }

    if (id.startsWith("item:")) {
      const itemId = id.slice("item:".length);
      if (overId?.startsWith("folder:")) {
        const folderId = overId.slice("folder:".length);
        if (folderId !== itemId) {
          actions.dropFileIntoFolder(folderId, itemId);
          return;
        }
      }
      const surface = state.surfaces[state.shell.activeDeskId];
      const pos = surface?.positions[itemId];
      if (!pos) return;
      const target: GridPos = {
        col: pos.col + Math.round(event.delta.x / CELL_W),
        row: pos.row + Math.round(event.delta.y / CELL_H),
      };
      actions.moveSurfaceItem(itemId, target);
    }
  }

  // ---- context menus ----
  function deskMenuEntries(cell: GridPos): MenuEntry[] {
    const widgets = registry.widgets;
    return [
      {
        id: "new-folder",
        label: "New folder",
        onSelect: () => actions.createFolder("New folder", cell),
      },
      ...widgets.map((w) => ({
        id: `add-${w.id}`,
        label: `Add ${w.name} widget`,
        onSelect: () => void actions.addWidget(w.id),
      })),
      { id: "sort", label: "Sort by name", onSelect: () => actions.sortActiveDesk() },
    ];
  }

  function menuEntriesFor(m: DeskMenu): MenuEntry[] {
    if (m.kind === "desk") return deskMenuEntries(m.cell);
    const item = state.surfaces[state.shell.activeDeskId]?.items[m.itemId ?? ""];
    return item ? itemMenuEntries(item) : [];
  }

  function itemMenuEntries(item: DesktopItem): MenuEntry[] {
    if (item.kind === "file") {
      return [
        {
          id: "open",
          label: "Open in VS Code",
          disabled: !item.artifactId || !!item.uploadState,
          onSelect: () => openFile(item),
        },
        {
          id: "remove",
          label: "Remove from desk",
          onSelect: () => actions.removeSurfaceItem(item.id),
        },
      ];
    }
    if (item.kind === "folder") {
      return [
        { id: "open", label: "Open", onSelect: () => setOpenFolderId(item.id) },
        { id: "delete", label: "Delete folder", onSelect: () => actions.deleteFolder(item.id) },
      ];
    }
    return [{ id: "remove", label: "Remove from desk", onSelect: () => actions.removeWidget(item.id) }];
  }

  return (
    <DndContext
      sensors={sensors}
      collisionDetection={pointerWithin}
      onDragStart={onDragStart}
      onDragEnd={onDragEnd}
    >
      <div className="os-desktop" data-os-desktop data-dragging-window={draggingWindow || undefined}>
        <MemoryField />
        <div
          className="os-plates"
          style={{ transform: `translateX(${-activeIndex * 100}%)` }}
          aria-live="off"
        >
          {desks.map((desk, index) => (
            <DeskPlate
              key={desk.id}
              desk={desk}
              index={index}
              active={index === activeIndex}
              viewport={viewport}
              placement={placement}
              surface={state.surfaces[desk.id]}
              selectedId={selectedId}
              openFolderId={openFolderId}
              noAnswerFor={noAnswerFor}
              actorRole={actorRole}
              onSelect={setSelectedId}
              onOpenFile={openFile}
              onRetryUpload={(itemId) => {
                const file = pendingFiles.current.get(itemId);
                if (file) startUpload(file, { col: 0, row: 0 }, itemId);
                else actions.removeSurfaceItem(itemId);
              }}
              onToggleFolder={(id) => setOpenFolderId((v) => (v === id ? null : id))}
              onMenu={(m) => setMenu(m)}
              onHostDrop={onHostDrop}
              onBackgroundClick={() => {
                setSelectedId(null);
                setMenu(null);
              }}
            />
          ))}
        </div>
        <p className="os-sr-only" aria-live="polite">
          Desk {activeIndex + 1} of {desks.length}
        </p>
        <DeskPager draggingWindow={draggingWindow} />
        {menu ? (
          <ContextMenu
            x={menu.x}
            y={menu.y}
            label={menu.kind === "desk" ? "Desk" : "Item"}
            entries={menuEntriesFor(menu)}
            onClose={() => setMenu(null)}
          />
        ) : null}
      </div>
    </DndContext>
  );
}

function pointerXOf(event: DragEndEvent): number | null {
  const activator = event.activatorEvent as PointerEvent | undefined;
  if (activator && typeof activator.clientX === "number") {
    return activator.clientX + event.delta.x;
  }
  return null;
}

function pxToCell(event: React.DragEvent, grid: { cols: number; rows: number }): GridPos {
  const rect = (event.currentTarget as HTMLElement).getBoundingClientRect();
  return {
    col: Math.max(0, Math.min(grid.cols - 1, Math.floor((event.clientX - rect.left - SURFACE_PAD) / CELL_W))),
    row: Math.max(0, Math.min(grid.rows - 1, Math.floor((event.clientY - rect.top - SURFACE_PAD) / CELL_H))),
  };
}

function DeskPlate({
  desk,
  index,
  active,
  viewport,
  placement,
  surface,
  selectedId,
  openFolderId,
  noAnswerFor,
  actorRole,
  onSelect,
  onOpenFile,
  onRetryUpload,
  onToggleFolder,
  onMenu,
  onHostDrop,
  onBackgroundClick,
}: {
  desk: Desk;
  index: number;
  active: boolean;
  viewport: { w: number; h: number };
  placement: PlacementTokens;
  surface: DeskSurface | undefined;
  selectedId: string | null;
  openFolderId: string | null;
  noAnswerFor: string | null;
  actorRole: string;
  onSelect: (id: string | null) => void;
  onOpenFile: (item: Extract<DesktopItem, { kind: "file" }>) => void;
  onRetryUpload: (itemId: string) => void;
  onToggleFolder: (id: string) => void;
  onMenu: (menu: DeskMenu) => void;
  onHostDrop: (event: React.DragEvent) => void;
  onBackgroundClick: () => void;
}) {
  const { state, registry } = useOs();
  const { setNodeRef } = useDroppable({ id: active ? "surface" : `plate:${desk.id}` });

  const windows = desk.windows.flatMap((id) => {
    const win = state.shell.windows[id];
    return win ? [win] : [];
  });
  const rects = useMemo(
    () => placeWindows(windows, viewport, placement),
    [windows, viewport, placement],
  );
  const items = Object.entries(surface?.items ?? {});
  const empty = windows.length === 0 && items.length === 0;

  return (
    <div
      ref={setNodeRef}
      className="os-plate"
      data-os-desk={desk.id}
      aria-hidden={!active}
      onPointerDown={(event) => {
        if (event.target === event.currentTarget) onBackgroundClick();
      }}
      onContextMenu={(event) => {
        if (event.target !== event.currentTarget) return;
        event.preventDefault();
        onMenu({
          x: event.clientX,
          y: event.clientY,
          kind: "desk",
          cell: {
            col: Math.max(0, Math.floor((event.clientX - SURFACE_PAD) / CELL_W)),
            row: Math.max(0, Math.floor((event.clientY - SURFACE_PAD) / CELL_H)),
          },
        });
      }}
      onDragOver={(event) => {
        if (event.dataTransfer.types.includes("Files")) event.preventDefault();
      }}
      onDrop={onHostDrop}
    >
      <DeskNumeral index={index} />
      {items.map(([id, item]) => (
        <SurfaceItem
          key={id}
          item={item}
          pos={surface?.positions[id]}
          selected={selectedId === id}
          folderOpen={openFolderId === id}
          noAnswer={noAnswerFor === id}
          onSelect={() => onSelect(id)}
          onOpenFile={onOpenFile}
          onRetryUpload={() => onRetryUpload(id)}
          onToggleFolder={() => onToggleFolder(id)}
          onMenu={(x, y) => onMenu({ x, y, kind: "item", cell: { col: 0, row: 0 }, itemId: id })}
        />
      ))}
      {windows.map((win) => {
        const manifest = registry.apps.find((a) => a.id === win.appId);
        const rect = rects[win.id];
        if (!manifest || !rect) return null;
        return (
          <WindowFrame
            key={win.id}
            win={win}
            manifest={manifest}
            rect={rect}
            focused={state.shell.focusedWindowId === win.id}
            actorRole={actorRole}
          />
        );
      })}
      {empty && active ? (
        <p className="os-desk-hint">Drop a file, or open the Launcher.</p>
      ) : null}
    </div>
  );
}

function SurfaceItem({
  item,
  pos,
  selected,
  folderOpen,
  noAnswer,
  onSelect,
  onOpenFile,
  onRetryUpload,
  onToggleFolder,
  onMenu,
}: {
  item: DesktopItem;
  pos: GridPos | undefined;
  selected: boolean;
  folderOpen: boolean;
  noAnswer: boolean;
  onSelect: () => void;
  onOpenFile: (item: Extract<DesktopItem, { kind: "file" }>) => void;
  onRetryUpload: () => void;
  onToggleFolder: () => void;
  onMenu: (x: number, y: number) => void;
}) {
  const { actions, registry } = useOs();
  const { presence } = useMachines();
  const draggable = useDraggable({ id: `item:${item.id}` });
  const folderDrop = useDroppable({
    id: `folder:${item.id}`,
    disabled: item.kind !== "folder",
  });

  if (!pos) return null;
  const { left, top } = cellToPx(pos);
  const style: React.CSSProperties = {
    left,
    top,
    width: item.kind === "widget" ? item.w * CELL_W - 12 : undefined,
    height: item.kind === "widget" ? item.h * CELL_H - 12 : undefined,
    transform: draggable.transform
      ? `translate(${draggable.transform.x}px, ${draggable.transform.y}px)`
      : undefined,
  };

  const ref = (node: HTMLElement | null) => {
    draggable.setNodeRef(node);
    folderDrop.setNodeRef(node);
  };

  return (
    <div
      ref={ref}
      className="os-surface-item"
      data-kind={item.kind}
      data-dragging={draggable.isDragging || undefined}
      style={style}
      onContextMenu={(event) => {
        event.preventDefault();
        event.stopPropagation();
        onMenu(event.clientX, event.clientY);
      }}
      {...draggable.listeners}
    >
      {item.kind === "file" ? (
        <FileIcon
          entry={item}
          machine={item.producedByWorkerId ? presence(item.producedByWorkerId) : null}
          selected={selected}
          noAnswerMessage={noAnswer ? VSCODE_NO_ANSWER_MESSAGE : null}
          onOpen={() => onOpenFile(item)}
          onSelect={onSelect}
          onRetryUpload={onRetryUpload}
        />
      ) : item.kind === "folder" ? (
        <>
          <FolderIcon
            id={item.id}
            name={item.name}
            count={item.children.length}
            open={folderOpen}
            isDropTarget={folderDrop.isOver}
            onToggle={onToggleFolder}
            onRename={(name) => actions.renameFolder(item.id, name)}
          />
          {folderOpen ? (
            <FolderPopover folderId={item.id} entries={item.children} onOpenFile={onOpenFile} />
          ) : null}
        </>
      ) : (
        (() => {
          const manifest = widgetById(registry, item.widgetId);
          return manifest ? (
            <WidgetFrame manifest={manifest} onRemove={() => actions.removeWidget(item.id)} />
          ) : null;
        })()
      )}
    </div>
  );
}

function FolderPopover({
  folderId,
  entries,
  onOpenFile,
}: {
  folderId: string;
  entries: FileEntry[];
  onOpenFile: (item: Extract<DesktopItem, { kind: "file" }>) => void;
}) {
  const { actions } = useOs();
  return (
    <div className="os-folder-popover" role="group" aria-label="Folder contents">
      {entries.length === 0 ? <p className="os-caption">Empty. Drag files in.</p> : null}
      {entries.map((entry) => (
        <FolderEntry
          key={entry.id}
          folderId={folderId}
          entry={entry}
          takeOut={(f, id) => actions.takeFileOutOfFolder(f, id)}
          onOpen={() => onOpenFile({ kind: "file", ...entry })}
        />
      ))}
    </div>
  );
}

function FolderEntry({
  folderId,
  entry,
  takeOut,
  onOpen,
}: {
  folderId: string;
  entry: FileEntry;
  takeOut: (folderId: string, fileId: string) => void;
  onOpen: () => void;
}) {
  const drag = useDraggable({ id: `folderfile:${folderId}:${entry.id}` });
  return (
    <div
      ref={drag.setNodeRef}
      className="os-folder-entry"
      data-dragging={drag.isDragging || undefined}
      style={{
        transform: drag.transform ? `translate(${drag.transform.x}px, ${drag.transform.y}px)` : undefined,
      }}
      {...drag.listeners}
    >
      <button type="button" className="os-folder-entry-open" onDoubleClick={onOpen}>
        {entry.title}
      </button>
      <button
        type="button"
        className="os-link"
        aria-label={`Take ${entry.title} out of the folder`}
        onClick={() => takeOut(folderId, entry.id)}
      >
        Take out
      </button>
    </div>
  );
}
