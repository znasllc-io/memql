import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useDraggable, useDroppable, type DragEndEvent, type DragStartEvent } from "@dnd-kit/core";

import { newShortId } from "@znasllc-io/memql-sdk-core/client";

import { DeskFolderPopover } from "../apps/files/DeskFolderPopover";
import { TREE_PATH_SEP, uploadDroppedTree } from "../apps/files/uploadTree";
import { entriesOf, hasDirectory, walkEntries } from "../items/folderDrop";
import { browserHandoffPorts, openInVsCode, VSCODE_NO_ANSWER_MESSAGE, type HandoffPorts } from "../items/vscode";
import { FileIcon } from "../items/FileIcon";
import { FolderIcon } from "../items/FolderIcon";
import type { UploadProvider } from "../items/upload";
import { widgetById, widgetsForRole } from "../system/registry";
import { placeWindows, type PlacementTokens } from "../system/placement";
import type { DeskSurface, DesktopItem, GridPos } from "../system/desktop";
import type { Desk } from "../system/desks";
import { DeskNumeral, MemoryField } from "../wallpaper/MemoryField";
import { resolveThemePack } from "../themes/registry";
import { WidgetFrame } from "../widgets/WidgetFrame";
import { useMachines } from "../live/machines";
import { useOsConnection } from "../live/connection";
import { useSession } from "./access";
import { ContextMenu, type MenuEntry } from "./ContextMenu";
import { DeskPager } from "./DeskPager";
import { BIN_DROPPABLE_ID, type BinDropPayload } from "../apps/bin/concepts";
import { useShellDragClaim } from "./dragScope";
import { useOs } from "./state";
import { WindowFrame } from "./WindowFrame";

// The desktop (spec A/B): desk plates sliding horizontally, each carrying
// its surface (files, folders, widgets on the snap grid) and its windows
// at computed placements. Owns every drag (icons, window swap/throw), the
// desk + item context menus, host-file drop -> upload, and the VS Code
// handoff with its no-answer fallback.
//
// DESK FOLDERS ARE LIBRARY SHORTCUTS (design D4): create and rename here are
// Library mutations, the popover is a live view the popover itself retains,
// and remove-from-desk removes the shortcut only. The desk stays
// subscription-free until a popover opens.

// Stable by construction -- it is a dependency of the desk's registration
// effect, and an array rebuilt each render would re-register on every one.
const DESK_DRAG_PREFIXES = ["window:", "item:"] as const;

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
  // The wallpaper follows the theme, PREVIEW INCLUDED -- pointing at a card
  // in the marketplace has to restyle the memory field too, or the largest
  // surface on the screen is the one thing that does not change. Memoized on
  // the resolved pack because MemoryField rebuilds its lattice whenever these
  // options change identity, and a new object per render would regenerate the
  // field on every keystroke anywhere in the shell.
  const wallpaper = useMemo(
    () => resolveThemePack(state.previewPack ?? state.themePack, state.installedPacks).wallpaper,
    [state.previewPack, state.themePack, state.installedPacks],
  );
  const { config } = useSession();
  const connection = useOsConnection();
  const [menu, setMenu] = useState<DeskMenu | null>(null);
  const [openFolderId, setOpenFolderId] = useState<string | null>(null);
  const selectedId = state.selectedItemId;
  const [draggingWindow, setDraggingWindow] = useState(false);
  const [noAnswerFor, setNoAnswerFor] = useState<string | null>(null);
  // The desk's own error line (create/rename/move refusals): in-surface, at
  // the bottom of the desk, self-clearing -- the menu that caused it is gone
  // by the time the server answers, and a toast is not the house's way.
  const [deskError, setDeskError] = useState<string | null>(null);
  const pendingFiles = useRef(new Map<string, File>());
  const cancelHandoff = useRef<(() => void) | null>(null);


  const desks = state.shell.desks;
  const activeIndex = Math.max(0, desks.findIndex((d) => d.id === state.shell.activeDeskId));

  useEffect(() => () => cancelHandoff.current?.(), []);

  // ---- opening files: the VS Code handoff (spec D3) ----
  const openArtifact = useCallback(
    (artifactId: string, anchorId: string) => {
      if (!artifactId) return;
      cancelHandoff.current?.();
      setNoAnswerFor(null);
      cancelHandoff.current = openInVsCode(
        config.domain,
        artifactId,
        () => setNoAnswerFor(anchorId),
        handoffPorts,
      );
    },
    [config.domain, handoffPorts],
  );

  const openFile = useCallback(
    (item: Extract<DesktopItem, { kind: "file" }>) => {
      if (!item.artifactId || item.uploadState) return;
      openArtifact(item.artifactId, item.id);
    },
    [openArtifact],
  );

  useEffect(() => {
    if (!noAnswerFor) return;
    const t = setTimeout(() => setNoAnswerFor(null), 8000);
    return () => clearTimeout(t);
  }, [noAnswerFor]);

  useEffect(() => {
    if (!deskError) return;
    const t = setTimeout(() => setDeskError(null), 8000);
    return () => clearTimeout(t);
  }, [deskError]);

  const describe = (err: unknown): string => (err instanceof Error ? err.message : String(err));

  // ---- desk folders: Library mutations behind the desk's own gestures ----
  const createDeskFolder = useCallback(
    async (cell: GridPos) => {
      const query = connection?.query ?? null;
      if (query === null) {
        setDeskError("Not connected to the cluster, so no folder was created.");
        return;
      }
      const folderId = newShortId();
      const name = "New folder";
      try {
        await query.createLibraryFolder({ folderId, name });
        // The shortcut lands where the menu was opened; the folder row itself
        // arrives on the live feed like one created anywhere else.
        actions.placeFolderShortcut({ folderId, name }, cell);
      } catch (err: unknown) {
        setDeskError(describe(err));
      }
    },
    [connection, actions],
  );

  const renameDeskFolder = useCallback(
    async (itemId: string, folderId: string, name: string) => {
      const query = connection?.query ?? null;
      if (query === null) {
        setDeskError("Not connected to the cluster, so the folder keeps its name.");
        return;
      }
      try {
        await query.renameLibraryFolder({ folderId, name });
        actions.renameFolderShortcut(itemId, name);
      } catch (err: unknown) {
        setDeskError(describe(err));
      }
    },
    [connection, actions],
  );

  // Dropping a desk FILE onto a desk FOLDER files the artifact there (the
  // Drive model: one row update) and the standalone shortcut yields to the
  // folder -- the same reading the old icon-groups trained.
  const fileIntoFolder = useCallback(
    async (fileItem: Extract<DesktopItem, { kind: "file" }>, folderId: string) => {
      const query = connection?.query ?? null;
      if (query === null || fileItem.artifactId === "") return;
      try {
        await query.moveArtifactToFolder({ artifactId: fileItem.artifactId, folderId });
        actions.removeSurfaceItem(fileItem.id);
      } catch (err: unknown) {
        setDeskError(describe(err));
      }
    },
    [connection, actions],
  );

  // ---- host-file drop -> upload provider (spec B) ----
  const startUpload = useCallback(
    (file: File, preferred: GridPos, opts?: { existingItemId?: string; folderId?: string }) => {
      const itemId = opts?.existingItemId ?? `up-${Date.now()}-${file.name}`;
      const intoFolder = opts?.folderId !== undefined && opts.folderId !== "";
      // A file dropped ON a folder lands in that folder: no desk icon is
      // minted for it -- the folder's popover is where it appears, live.
      if (!intoFolder) {
        if (!opts?.existingItemId) {
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
      }
      uploads
        .upload(file, opts?.folderId ? { folderId: opts.folderId } : undefined)
        .done.then((result) => {
          if (intoFolder) return;
          pendingFiles.current.delete(itemId);
          actions.updateFileItem(itemId, {
            artifactId: result.artifactId,
            title: result.title,
            fileKind: result.fileKind,
            source: result.source,
            uploadState: undefined,
          });
        })
        .catch((err: unknown) => {
          if (intoFolder) {
            setDeskError(describe(err));
            return;
          }
          actions.updateFileItem(itemId, { uploadState: "failed" });
        });
    },
    [actions, uploads],
  );

  // A dropped DIRECTORY becomes a Library folder tree with a desk shortcut
  // to each top-level folder (design D3, desk half): the tree is created
  // first, the files stream into it, and the shortcut's popover shows them
  // landing live. Failures summarize on the desk's own error line.
  const dropTree = useCallback(
    async (event: React.DragEvent, cell: GridPos) => {
      const query = connection?.query ?? null;
      if (query === null) {
        setDeskError("Not connected to the cluster, so nothing was uploaded.");
        return;
      }
      const walked = await walkEntries(entriesOf(event.dataTransfer));
      if (walked.refusal !== "") {
        setDeskError(walked.refusal);
        return;
      }
      // Only the FILED half: loose files beside the directory already took
      // the ordinary icon path in onHostDrop, and uploading them here too
      // would land every one of them twice.
      const filed = walked.files.filter((f) => f.dirPath.length > 0);
      if (filed.length === 0) return;
      const result = await uploadDroppedTree(filed, "", {
        createFolder: async (name, parentFolderId) => {
          const folderId = newShortId();
          await query.createLibraryFolder({
            folderId,
            name,
            ...(parentFolderId !== "" ? { parentFolderId } : {}),
          });
          return folderId;
        },
        uploadFile: async (file, folderId) => {
          await uploads.upload(file, folderId !== "" ? { folderId } : undefined).done;
        },
        concurrency: 3,
        onFileSettled: () => {},
      });
      // A desk shortcut for each TOP-LEVEL folder (depth-1 path key), at the
      // drop cell outward. Its popover shows the files landing, live.
      for (const [key, folderId] of result.folderIdByPath) {
        if (key === "" || key.includes(TREE_PATH_SEP)) continue;
        actions.placeFolderShortcut({ folderId, name: key }, cell);
      }
      if (result.failures.length > 0) {
        const first = result.failures[0];
        setDeskError(
          `${result.failures.length} of ${filed.length} files did not land -- ${first?.error ?? ""} The landed files stay landed; the Files app lists the rest.`,
        );
      }
    },
    [connection, uploads, actions],
  );

  const onHostDrop = useCallback(
    (event: React.DragEvent) => {
      if (!event.dataTransfer.files.length && !event.dataTransfer.items.length) return;
      event.preventDefault();
      const cell = pxToCell(event, grid);
      const entries = entriesOf(event.dataTransfer);
      if (hasDirectory(entries)) {
        // Loose files beside the directory take the ordinary icon path; the
        // directory takes the tree path.
        for (const entry of entries) {
          if (!entry.isFile || !entry.file) continue;
          entry.file((file) => startUpload(file, cell));
        }
        void dropTree(event, cell);
        return;
      }
      for (const file of Array.from(event.dataTransfer.files)) startUpload(file, cell);
    },
    [grid, startUpload, dropTree],
  );

  const onFolderHostDrop = useCallback(
    (folderId: string, files: readonly File[]) => {
      for (const file of files) startUpload(file, { col: 0, row: 0 }, { folderId });
    },
    [startUpload],
  );

  // ---- shell drags: icons, folders, window swap/throw ----
  //
  // The DndContext lives at the shell root now (chrome/dragScope.tsx), because
  // the Bin is in the DOCK and a drag begun here has to be able to reach it.
  // The desk claims its own id prefixes; the dock claims `pin:`; neither can
  // see the other's drops.
  useShellDragClaim(DESK_DRAG_PREFIXES, { onDragStart, onDragEnd });

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

    if (id.startsWith("item:")) {
      const itemId = id.slice("item:".length);
      const surface = state.surfaces[state.shell.activeDeskId];
      // The Bin owns this drop (epic memql#4784). It is the dock's droppable,
      // reachable now that one DndContext spans the shell, and the desk
      // deliberately does not act: archiving is a write with a confirm and a
      // refusal to render, and both live where the gesture ended.
      if (overId === BIN_DROPPABLE_ID) return;
      if (overId?.startsWith("folder:")) {
        const folderItemId = overId.slice("folder:".length);
        const folderItem = surface?.items[folderItemId];
        const fileItem = surface?.items[itemId];
        if (
          folderItemId !== itemId &&
          folderItem?.kind === "folder" &&
          fileItem?.kind === "file" &&
          !fileItem.uploadState
        ) {
          void fileIntoFolder(fileItem, folderItem.folderId);
          return;
        }
      }
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
    // ROLE-FILTERED, like the launcher's Widgets tab (epic memql#4832, D1).
    // This read the unfiltered `registry.widgets`, so a gated widget would
    // have been offered here to everyone. The action refuses it too -- an
    // offer nobody can take is a worse bug than a missing offer, but a menu
    // that offers it at all is the one the person actually sees.
    const widgets = widgetsForRole(registry, actorRole);
    return [
      {
        id: "new-folder",
        label: "New folder",
        // A desk folder IS a Library folder now, so creating one is a write
        // the cluster must confirm; with no connection the entry says so by
        // refusing rather than minting a shortcut to nothing.
        disabled: connection === null,
        onSelect: () => void createDeskFolder(cell),
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
        {
          id: "remove",
          // The shortcut goes; the Library folder and everything in it stay.
          // Archiving lives in the Files app, where the confirm can name the
          // live count.
          label: "Remove from desk",
          onSelect: () => actions.removeSurfaceItem(item.id),
        },
      ];
    }
    return [{ id: "remove", label: "Remove from desk", onSelect: () => actions.removeWidget(item.id) }];
  }

  return (
    <>
      <div className="os-desktop" data-os-desktop data-dragging-window={draggingWindow || undefined}>
        <MemoryField seed={wallpaper.seed} field={wallpaper} />
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
              onSelect={actions.selectSurfaceItem}
              onOpenFile={openFile}
              onOpenArtifact={openArtifact}
              onRetryUpload={(itemId) => {
                const file = pendingFiles.current.get(itemId);
                if (file) startUpload(file, { col: 0, row: 0 }, { existingItemId: itemId });
                else actions.removeSurfaceItem(itemId);
              }}
              onToggleFolder={(id) => setOpenFolderId((v) => (v === id ? null : id))}
              onRenameFolder={renameDeskFolder}
              onFolderHostDrop={onFolderHostDrop}
              onMenu={(m) => setMenu(m)}
              onHostDrop={onHostDrop}
              onBackgroundClick={() => {
                actions.selectSurfaceItem(null);
                setMenu(null);
              }}
            />
          ))}
        </div>
        <p className="os-sr-only" aria-live="polite">
          Desk {activeIndex + 1} of {desks.length}
        </p>
        {deskError ? (
          <p className="os-desk-error" role="alert">
            {deskError}
          </p>
        ) : null}
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
    </>
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
  onOpenArtifact,
  onRetryUpload,
  onToggleFolder,
  onRenameFolder,
  onFolderHostDrop,
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
  onOpenArtifact: (artifactId: string, anchorId: string) => void;
  onRetryUpload: (itemId: string) => void;
  onToggleFolder: (id: string) => void;
  onRenameFolder: (itemId: string, folderId: string, name: string) => void;
  onFolderHostDrop: (folderId: string, files: readonly File[]) => void;
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
          noAnswerFor={noAnswerFor}
          onSelect={() => onSelect(id)}
          onOpenFile={onOpenFile}
          onOpenArtifact={onOpenArtifact}
          onRetryUpload={() => onRetryUpload(id)}
          onToggleFolder={() => onToggleFolder(id)}
          onRenameFolder={onRenameFolder}
          onFolderHostDrop={onFolderHostDrop}
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
  noAnswerFor,
  onSelect,
  onOpenFile,
  onOpenArtifact,
  onRetryUpload,
  onToggleFolder,
  onRenameFolder,
  onFolderHostDrop,
  onMenu,
}: {
  item: DesktopItem;
  pos: GridPos | undefined;
  selected: boolean;
  folderOpen: boolean;
  noAnswerFor: string | null;
  onSelect: () => void;
  onOpenFile: (item: Extract<DesktopItem, { kind: "file" }>) => void;
  onOpenArtifact: (artifactId: string, anchorId: string) => void;
  onRetryUpload: () => void;
  onToggleFolder: () => void;
  onRenameFolder: (itemId: string, folderId: string, name: string) => void;
  onFolderHostDrop: (folderId: string, files: readonly File[]) => void;
  onMenu: (x: number, y: number) => void;
}) {
  const { actions, registry } = useOs();
  const { presence } = useMachines();
  // The drag carries what the Bin needs to name and archive this, because the
  // dock holds neither the Library feed nor the desktop document (memql#4784).
  const draggable = useDraggable({
    id: `item:${item.id}`,
    data: {
      artifactId: item.kind === "file" ? (item.artifactId ?? "") : "",
      name: item.kind === "file" ? item.title || item.id : item.kind === "folder" ? item.name : item.id,
      folder: item.kind === "folder",
      deskItemId: item.id,
    } satisfies BinDropPayload,
  });
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
      {...(item.kind === "folder"
        ? {
            // A HOST file dropped on a folder lands IN that folder. Both
            // phases stop propagation (the Training rule): the desk plate
            // underneath takes file drops too, and without the stop one drop
            // would upload twice -- once into the folder, once onto the desk.
            onDragOver: (event: React.DragEvent) => {
              if (!event.dataTransfer.types.includes("Files")) return;
              event.preventDefault();
              event.stopPropagation();
            },
            onDrop: (event: React.DragEvent) => {
              if (!event.dataTransfer.files.length) return;
              event.preventDefault();
              event.stopPropagation();
              onFolderHostDrop(item.folderId, Array.from(event.dataTransfer.files));
            },
          }
        : {})}
      {...draggable.listeners}
    >
      {item.kind === "file" ? (
        <FileIcon
          entry={item}
          machine={item.producedByWorkerId ? presence(item.producedByWorkerId) : null}
          selected={selected}
          noAnswerMessage={noAnswerFor === item.id ? VSCODE_NO_ANSWER_MESSAGE : null}
          onOpen={() => onOpenFile(item)}
          onSelect={onSelect}
          onRetryUpload={onRetryUpload}
        />
      ) : item.kind === "folder" ? (
        <>
          <FolderIcon
            id={item.id}
            name={item.name}
            open={folderOpen}
            isDropTarget={folderDrop.isOver}
            onToggle={onToggleFolder}
            onRename={(name) => onRenameFolder(item.id, item.folderId, name)}
          />
          {folderOpen ? (
            <DeskFolderPopover
              folderId={item.folderId}
              noAnswerFor={noAnswerFor}
              onOpen={onOpenArtifact}
            />
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
