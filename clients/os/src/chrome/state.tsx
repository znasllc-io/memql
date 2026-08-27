// The shell's one state hub: the kernel's ShellState + per-desk surfaces +
// dock pins + theme pack, persisted through DesktopStore on every change.
// Components call the actions; nothing outside this file mutates state.

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";

import {
  addDesk as addDeskFn,
  closeWindow as closeWindowFn,
  focusWindow as focusWindowFn,
  gcAutoDesks,
  initialShell,
  nextId,
  openApp as openAppFn,
  setWindowMode,
  setWindowSection,
  swapSides as swapSidesFn,
  switchDesk as switchDeskFn,
  switchDeskBy as switchDeskByFn,
  throwToDesk as throwToDeskFn,
  type ShellEffect,
  type ShellState,
} from "../system/desks";
import {
  addFileToFolder as addFileToFolderFn,
  addItem,
  createFolder as createFolderFn,
  deleteFolder as deleteFolderFn,
  emptySurface,
  moveItem as moveItemFn,
  removeFileFromFolder as removeFileFromFolderFn,
  removeItem as removeItemFn,
  renameFolder as renameFolderFn,
  sortSurface,
  surfaceHasContent,
  updateFile as updateFileFn,
  type DeskSurface,
  type DesktopItem,
  type FileEntry,
  type GridPos,
  type GridSize,
} from "../system/desktop";
import { movePin as movePinFn, pin as pinFn, unpin as unpinFn, type DockState } from "../system/dock";
import { canOpen, widgetById, type OsRegistry } from "../system/registry";
import { roleAdmits } from "../system/roles";
import {
  documentFromState,
  LocalDesktopStore,
  type DesktopDocument,
  type DesktopStore,
} from "../system/store";
import type { AppId, DeskId, WindowId } from "../system/windows";

export interface OsState {
  shell: ShellState;
  surfaces: Record<DeskId, DeskSurface>;
  dock: DockState;
  themePack: string;
}

export interface OsActions {
  openApp: (appId: AppId) => ShellEffect;
  closeWindow: (id: WindowId) => void;
  minimizeWindow: (id: WindowId) => void;
  toggleFullscreen: (id: WindowId) => void;
  focusWindow: (id: WindowId) => void;
  navigateSection: (id: WindowId, sectionId: string) => void;
  swapSides: (deskId: DeskId) => void;
  throwToDesk: (id: WindowId, target: DeskId | "new") => ShellEffect;
  switchDesk: (deskId: DeskId) => void;
  switchDeskBy: (delta: 1 | -1) => void;
  addDesk: () => void;
  pinApp: (appId: AppId) => void;
  unpinApp: (appId: AppId) => void;
  movePin: (appId: AppId, toIndex: number) => void;
  addFile: (entry: FileEntry, preferred: GridPos) => boolean;
  updateFileItem: (itemId: string, patch: Partial<Omit<FileEntry, "id">>) => void;
  removeSurfaceItem: (itemId: string) => void;
  moveSurfaceItem: (itemId: string, to: GridPos) => void;
  createFolder: (name: string, preferred: GridPos) => void;
  renameFolder: (folderId: string, name: string) => void;
  deleteFolder: (folderId: string) => void;
  dropFileIntoFolder: (folderId: string, fileItemId: string) => void;
  takeFileOutOfFolder: (folderId: string, fileId: string) => void;
  addWidget: (widgetId: string) => boolean;
  removeWidget: (itemId: string) => void;
  sortActiveDesk: () => void;
  setThemePack: (pack: string) => void;
}

export interface OsContextValue {
  state: OsState;
  actions: OsActions;
  registry: OsRegistry;
  actorRole: string;
  grid: GridSize;
}

const OsContext = createContext<OsContextValue | null>(null);

export function useOs(): OsContextValue {
  const value = useContext(OsContext);
  if (!value) throw new Error("useOs outside OsProvider");
  return value;
}

/**
 * Grid size from the viewport (cell tokens are 96x104). The surface pads
 * 20px on each side and the dock reserves the bottom -- both come OUT of
 * the cell budget, or the last column paints past the viewport edge and
 * bleeds onto the neighboring desk plate.
 */
export function gridForViewport(width: number, height: number): GridSize {
  return {
    cols: Math.max(3, Math.floor((width - 40) / 96)),
    rows: Math.max(2, Math.floor((height - 200) / 104)),
  };
}

/**
 * First-run document: one desk, the Ask widget resting top-right (the one
 * pre-placed thing a fresh desktop carries), Settings pinned so the dock
 * is never empty.
 */
export function seedDocument(registry: OsRegistry, grid: GridSize): OsState {
  const shell = initialShell();
  let surface = emptySurface();
  const ask = widgetById(registry, "ask");
  if (ask) {
    const placed = addItem(
      surface,
      { kind: "widget", id: nextId("item"), widgetId: ask.id, w: ask.size.w, h: ask.size.h },
      { col: Math.max(0, grid.cols - ask.size.w), row: 0 },
      grid,
    );
    if (placed) surface = placed;
  }
  return {
    shell,
    surfaces: { [shell.activeDeskId]: surface },
    dock: { pinned: ["settings"] },
    themePack: "graphite",
  };
}

function stateFromDocument(doc: DesktopDocument): OsState {
  const shell: ShellState = {
    desks: doc.desks.map((d) => ({ ...d, windows: [] })),
    activeDeskId: doc.activeDeskId,
    windows: {},
    focusedWindowId: null,
  };
  return { shell, surfaces: doc.surfaces, dock: doc.dock, themePack: doc.themePack };
}

export function OsProvider({
  children,
  registry,
  actorRole,
  store,
  grid,
}: {
  children: ReactNode;
  registry: OsRegistry;
  actorRole: string;
  store?: DesktopStore;
  grid: GridSize;
}) {
  const storeRef = useRef<DesktopStore>(store ?? new LocalDesktopStore());
  const [state, setState] = useState<OsState>(() => {
    const loaded = storeRef.current.load();
    return loaded ? stateFromDocument(loaded) : seedDocument(registry, grid);
  });

  // Persist on every settled change. The document never carries windows,
  // so persisting during interaction is cheap and safe.
  useEffect(() => {
    storeRef.current.save(documentFromState(state.shell, state.surfaces, state.dock, state.themePack));
  }, [state]);

  const surfaceOf = useCallback(
    (s: OsState, deskId: DeskId): DeskSurface => s.surfaces[deskId] ?? emptySurface(),
    [],
  );

  const withSurface = useCallback(
    (s: OsState, deskId: DeskId, surface: DeskSurface | null): OsState =>
      surface ? { ...s, surfaces: { ...s.surfaces, [deskId]: surface } } : s,
    [],
  );

  const hasContent = useCallback(
    (s: OsState) => (deskId: DeskId) => surfaceHasContent(s.surfaces[deskId]),
    [],
  );

  /** Drop surfaces whose desk was garbage-collected. */
  const pruneSurfaces = useCallback((s: OsState): OsState => {
    const deskIds = new Set(s.shell.desks.map((d) => d.id));
    const surfaces = Object.fromEntries(Object.entries(s.surfaces).filter(([id]) => deskIds.has(id)));
    return Object.keys(surfaces).length === Object.keys(s.surfaces).length ? s : { ...s, surfaces };
  }, []);

  const gridRef = useRef(grid);
  gridRef.current = grid;

  const actionsRef = useRef<OsActions | null>(null);
  if (!actionsRef.current) {
    const set = (updater: (s: OsState) => OsState) => setState((s) => pruneSurfaces(updater(s)));
    let lastEffect: ShellEffect = { kind: "none" };

    actionsRef.current = {
      openApp: (appId) => {
        lastEffect = { kind: "none" };
        set((s) => {
          if (!canOpen(registry, actorRoleRef.current, appId)) return s;
          const { state: shell, effect } = openAppFn(s.shell, appId, defaultSection(registry, actorRoleRef.current, appId));
          lastEffect = effect;
          return { ...s, shell };
        });
        return lastEffect;
      },
      closeWindow: (id) => set((s) => ({ ...s, shell: closeWindowFn(s.shell, id).state })),
      minimizeWindow: (id) => set((s) => ({ ...s, shell: setWindowMode(s.shell, id, "minimized") })),
      toggleFullscreen: (id) =>
        set((s) => ({
          ...s,
          shell: setWindowMode(s.shell, id, s.shell.windows[id]?.mode === "fullscreen" ? "normal" : "fullscreen"),
        })),
      focusWindow: (id) => set((s) => ({ ...s, shell: focusWindowFn(s.shell, id) })),
      navigateSection: (id, sectionId) => set((s) => ({ ...s, shell: setWindowSection(s.shell, id, sectionId) })),
      swapSides: (deskId) => set((s) => ({ ...s, shell: swapSidesFn(s.shell, deskId) })),
      throwToDesk: (id, target) => {
        lastEffect = { kind: "none" };
        set((s) => {
          const { state: shell, effect } = throwToDeskFn(s.shell, id, target);
          lastEffect = effect;
          return { ...s, shell };
        });
        return lastEffect;
      },
      switchDesk: (deskId) =>
        set((s) => ({ ...s, shell: switchDeskFn(s.shell, deskId, { deskHasSurfaceContent: hasContent(s) }) })),
      switchDeskBy: (delta) =>
        set((s) => ({ ...s, shell: switchDeskByFn(s.shell, delta, { deskHasSurfaceContent: hasContent(s) }) })),
      addDesk: () => set((s) => ({ ...s, shell: addDeskFn(s.shell) })),
      pinApp: (appId) => set((s) => ({ ...s, dock: pinFn(s.dock, appId) })),
      unpinApp: (appId) => set((s) => ({ ...s, dock: unpinFn(s.dock, appId) })),
      movePin: (appId, toIndex) => set((s) => ({ ...s, dock: movePinFn(s.dock, appId, toIndex) })),
      addFile: (entry, preferred) => {
        let ok = false;
        set((s) => {
          const deskId = s.shell.activeDeskId;
          const placed = addItem(surfaceOf(s, deskId), { kind: "file", ...entry }, preferred, gridRef.current);
          ok = !!placed;
          return withSurface(s, deskId, placed);
        });
        return ok;
      },
      updateFileItem: (itemId, patch) =>
        set((s) => {
          const deskId = deskOfItem(s, itemId);
          return deskId ? withSurface(s, deskId, updateFileFn(surfaceOf(s, deskId), itemId, patch)) : s;
        }),
      removeSurfaceItem: (itemId) =>
        set((s) => {
          const deskId = deskOfItem(s, itemId);
          return deskId ? withSurface(s, deskId, removeItemFn(surfaceOf(s, deskId), itemId)) : s;
        }),
      moveSurfaceItem: (itemId, to) =>
        set((s) => {
          const deskId = s.shell.activeDeskId;
          return withSurface(s, deskId, moveItemFn(surfaceOf(s, deskId), itemId, to, gridRef.current));
        }),
      createFolder: (name, preferred) =>
        set((s) => {
          const deskId = s.shell.activeDeskId;
          return withSurface(s, deskId, createFolderFn(surfaceOf(s, deskId), nextId("item"), name, preferred, gridRef.current));
        }),
      renameFolder: (folderId, name) =>
        set((s) => {
          const deskId = deskOfItem(s, folderId);
          return deskId ? withSurface(s, deskId, renameFolderFn(surfaceOf(s, deskId), folderId, name)) : s;
        }),
      deleteFolder: (folderId) =>
        set((s) => {
          const deskId = deskOfItem(s, folderId);
          return deskId ? withSurface(s, deskId, deleteFolderFn(surfaceOf(s, deskId), folderId, gridRef.current)) : s;
        }),
      dropFileIntoFolder: (folderId, fileItemId) =>
        set((s) => {
          const deskId = deskOfItem(s, folderId);
          return deskId ? withSurface(s, deskId, addFileToFolderFn(surfaceOf(s, deskId), folderId, fileItemId)) : s;
        }),
      takeFileOutOfFolder: (folderId, fileId) =>
        set((s) => {
          const deskId = deskOfItem(s, folderId);
          return deskId ? withSurface(s, deskId, removeFileFromFolderFn(surfaceOf(s, deskId), folderId, fileId, gridRef.current)) : s;
        }),
      addWidget: (widgetId) => {
        let ok = false;
        set((s) => {
          const manifest = widgetById(registry, widgetId);
          if (!manifest) return s;
          const deskId = s.shell.activeDeskId;
          const surface = surfaceOf(s, deskId);
          const already = Object.values(surface.items).some(
            (i) => i.kind === "widget" && i.widgetId === widgetId,
          );
          if (already) {
            ok = true;
            return s;
          }
          const item: DesktopItem = {
            kind: "widget",
            id: nextId("item"),
            widgetId,
            w: manifest.size.w,
            h: manifest.size.h,
          };
          const placed = addItem(surface, item, { col: 0, row: 0 }, gridRef.current);
          ok = !!placed;
          return withSurface(s, deskId, placed);
        });
        return ok;
      },
      removeWidget: (itemId) => actionsRef.current!.removeSurfaceItem(itemId),
      sortActiveDesk: () =>
        set((s) => {
          const deskId = s.shell.activeDeskId;
          return withSurface(s, deskId, sortSurface(surfaceOf(s, deskId), gridRef.current));
        }),
      setThemePack: (pack) => set((s) => ({ ...s, themePack: pack })),
    };
  }

  // actorRole can change after a refresh of MyAccess facts; keep the
  // stable actions object reading the current value.
  const actorRoleRef = useRef(actorRole);
  actorRoleRef.current = actorRole;

  const value = useMemo<OsContextValue>(
    () => ({ state, actions: actionsRef.current!, registry, actorRole, grid }),
    [state, registry, actorRole, grid],
  );

  useEffect(() => {
    document.documentElement.setAttribute("data-os-theme", state.themePack);
  }, [state.themePack]);

  return <OsContext.Provider value={value}>{children}</OsContext.Provider>;
}

function deskOfItem(s: OsState, itemId: string): DeskId | null {
  for (const [deskId, surface] of Object.entries(s.surfaces)) {
    if (surface.items[itemId]) return deskId;
  }
  return null;
}

function defaultSection(registry: OsRegistry, actorRole: string, appId: AppId): string {
  const app = registry.apps.find((a) => a.id === appId);
  if (!app?.sections?.length) return "";
  const admitted = app.sections.filter((sec) => roleAdmits(actorRole, sec.roles));
  return admitted[0]?.id ?? "";
}

/** GC pass exposed for tests: prune desks then surfaces coherently. */
export function gcState(state: OsState): OsState {
  const shell = gcAutoDesks(state.shell, (deskId) => surfaceHasContent(state.surfaces[deskId]));
  const deskIds = new Set(shell.desks.map((d) => d.id));
  const surfaces = Object.fromEntries(Object.entries(state.surfaces).filter(([id]) => deskIds.has(id)));
  return { ...state, shell, surfaces };
}
