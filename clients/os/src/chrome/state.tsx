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
  consumeIntent as consumeIntentFn,
  focusWindow as focusWindowFn,
  gcAutoDesks,
  initialShell,
  nextId,
  nextIdAvoiding,
  openApp as openAppFn,
  setWindowMode,
  setWindowSection,
  swapSides as swapSidesFn,
  switchDesk as switchDeskFn,
  switchDeskBy as switchDeskByFn,
  throwToDesk as throwToDeskFn,
  toggleFullscreen as toggleFullscreenFn,
  type ShellEffect,
  type ShellState,
} from "../system/desks";
import {
  addItem,
  emptySurface,
  moveItem as moveItemFn,
  removeItem as removeItemFn,
  sortSurface,
  surfaceHasContent,
  updateFile as updateFileFn,
  updateFolder as updateFolderFn,
  type DeskSurface,
  type DesktopItem,
  type FileEntry,
  type GridPos,
  type GridSize,
} from "../system/desktop";
import { movePin as movePinFn, pin as pinFn, unpin as unpinFn, type DockState } from "../system/dock";
import { accessAdmits, canOpen, sectionsFor, widgetById, type OsRegistry } from "../system/registry";
import {
  documentFromState,
  LocalDesktopStore,
  type DesktopDocument,
  type DesktopStore,
} from "../system/store";
import type { AppId, DeskId, WindowId } from "../system/windows";
import { applyPackStyles, BUILT_IN_THEME_ID, type OsThemePack } from "../themes/registry";
import type { ChromeLayout } from "../app/layout";

export interface OsState {
  shell: ShellState;
  surfaces: Record<DeskId, DeskSurface>;
  dock: DockState;
  themePack: string;
  /** Theme packs installed on this desktop. Built-ins are not in here. */
  installedPacks: OsThemePack[];
  /**
   * A pack being LOOKED AT, not chosen. Session state, deliberately absent
   * from `documentFromState`: the marketplace previews by applying a pack to
   * the real desktop while the pointer is over its card, and a preview that
   * reached the store would roam somebody else's machine to a theme nobody
   * picked.
   *
   * It is state rather than a direct attribute write because the effect that
   * stamps `data-os-theme` is keyed on this state -- a write from a pointer
   * handler would be reverted by the next render, intermittently.
   */
  previewPack: string | null;
  /**
   * The selected desk item. SESSION state, exactly like windows: it lives
   * here rather than inside the Desktop component so an app surface can
   * FOCUS an item ("send to desktop" of something already present), and it
   * is deliberately absent from `documentFromState` -- a roamed desktop must
   * not move another machine's selection.
   */
  selectedItemId: string | null;
}

export interface OsActions {
  /**
   * Open an app, focusing its existing window when it is already running.
   *
   * `sectionId` is the section to open a NEW window ON, and it is not the
   * same thing as navigating afterwards: an app applies its own
   * default-section preference on mount (Fleet does), so a window created on
   * the shell default and navigated a tick later gets dragged back. Opening
   * it on the requested section is what lets the app tell "the shell put me
   * here" apart from "somebody asked for this".
   *
   * `payload` (epic memql#4842, #4845) rides to the app as a consumable
   * window intent -- the id is minted here, so callers hand over only what
   * they want shown. Delivered to a fresh window and an already-open one
   * alike (replacing any standing intent and adopting the section).
   *
   * Omitted = the shell default (the first role-admitted section).
   */
  openApp: (appId: AppId, sectionId?: string, payload?: Record<string, unknown>) => ShellEffect;
  /** Clear a window's consumed intent, matched by id (stale consumes no-op). */
  consumeWindowIntent: (windowId: WindowId, intentId: string) => void;
  closeWindow: (id: WindowId) => void;
  minimizeWindow: (id: WindowId) => void;
  toggleFullscreen: (id: WindowId) => void;
  focusWindow: (id: WindowId) => void;
  navigateSection: (id: WindowId, sectionId: string, origin?: "peer" | "content" | "back") => void;
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
  /** Select a desk item (null clears). Session state, never persisted. */
  selectSurfaceItem: (itemId: string | null) => void;
  /**
   * Put a Library FILE on the active desk, or focus it if a shortcut to the
   * same artifact is already there -- the dedupe rule, in the one place it
   * can live now that apps can send too.
   */
  sendFileToDesk: (entry: Omit<FileEntry, "id">) => "placed" | "focused" | "full";
  /** The folder-shortcut sibling of sendFileToDesk, deduped by folderId. */
  sendFolderToDesk: (folderId: string, name: string) => "placed" | "focused" | "full";
  /** Place a folder shortcut at a cell (desk create-folder, after the
   *  Library mutation landed). */
  placeFolderShortcut: (shortcut: { folderId: string; name: string }, preferred: GridPos) => boolean;
  /** Refresh a folder shortcut's denormalized name from live rows. */
  renameFolderShortcut: (itemId: string, name: string) => void;
  addWidget: (widgetId: string) => boolean;
  /**
   * Put a widget on the ACTIVE desk if it is not already there, seated under
   * `under` when that widget is on the desk too.
   *
   * The DERIVED half of the setup wizard's presence (epic memql#5118, D4).
   * Unlike `addWidget` it reports nothing and does nothing when the widget is
   * already present, because its caller is an effect that re-runs on every
   * change of the readiness feed -- a boolean nobody reads would only invite
   * somebody to branch on "did it land this time".
   *
   * IDEMPOTENT BY THE DESK'S CONTENTS, never by a remembered flag: anything
   * remembered would be a second answer to a question the desk can already
   * answer, and it would be wrong the moment somebody moved to another desk.
   */
  ensureWidget: (widgetId: string, under?: string) => void;
  removeWidget: (itemId: string) => void;
  sortActiveDesk: () => void;
  setThemePack: (pack: string) => void;
  /** Add a validated pack, replacing any earlier one with the same id. */
  installThemePack: (pack: OsThemePack) => void;
  /** Remove an installed pack. A desktop wearing it falls back to graphite. */
  removeThemePack: (id: string) => void;
  /** Look at a pack without choosing it. Null ends the preview. */
  previewThemePack: (id: string | null) => void;
}

/**
 * A quiet report from the store about something the person did not do here.
 *
 * `roamed` -- another machine saved this desktop and the shell has taken it
 * on. `stale` -- the stored desktop is from a newer version of this app, so
 * this session has stopped writing to it; a reload is the fix.
 *
 * `roamed` clears itself; `stale` does NOT, because it describes a condition
 * that is still true a minute later.
 */
export type OsNotice = { kind: "roamed" } | { kind: "stale" };

export interface OsContextValue {
  state: OsState;
  actions: OsActions;
  registry: OsRegistry;
  actorRole: string;
  /**
   * Whether the cluster's role LADDER has loaded (epic memql#4832, memql#4857).
   *
   * THE REACTIVITY SIGNAL FOR EVERY roleAdmits-CONSUMING SURFACE. The ladder
   * is async module state that lands AFTER the role (a slow `activeRoles`
   * read), and roleAdmits reads it out of band -- so a launcher/dock memo
   * keyed only on `actorRole` computes its app list against an EMPTY ladder,
   * refuses every gated app fail-closed, and never recomputes when the ladder
   * arrives (actorRole did not change). Carrying the flag here, and naming it
   * in those memos' deps, is what makes them recompute the moment it flips.
   * A surface that filters by role MUST depend on this.
   */
  ladderLoaded: boolean;
  /**
   * The effective capability set's epoch (epic memql#5289). THE REACTIVITY
   * SIGNAL FOR EVERY CAPABILITY-FILTERING SURFACE, the way `ladderLoaded` is
   * for the rank questions: `appsFor`, `sectionsFor`, `canOpen` and `holds`
   * read the set out of band, so a memo that does not name this recomputes
   * never. It moves on the first read, on every focus re-read and after a
   * grant written from this browser (D11).
   */
  accessEpoch: number;
  grid: GridSize;
  /**
   * Which chrome is drawn. Here because an act that OPENS A WINDOW is only
   * offerable where windows exist: the phone shell has none, so a Set up
   * group there names the destination in words instead of a button that
   * could not go anywhere. Read it as `!== "phone"`, never `=== "desktop"` --
   * the iPad chrome carries windows too.
   */
  layout: ChromeLayout;
  /** Null when there is nothing to report. Rendered by the dock. */
  notice: OsNotice | null;
}

const OsContext = createContext<OsContextValue | null>(null);

export function useOs(): OsContextValue {
  const value = useContext(OsContext);
  if (!value) throw new Error("useOs outside OsProvider");
  return value;
}

/**
 * The shell, or null when this tree is not inside one.
 *
 * `useOs` THROWS, which is right for chrome: the dock, the desk and a
 * window frame have no meaning outside a shell, so a missing provider
 * there is a wiring bug worth failing loudly on.
 *
 * An APP is the other case. Most of what an app does -- read rows, draw
 * them, write them back -- needs no shell at all; the one thing that does
 * is handing off to ANOTHER app, which happens on a click. Reaching for
 * `useOs` at an app's root to have `openApp` available for that click
 * makes the whole app unmountable without the entire desktop, which costs
 * every one of its tests a shell it does not otherwise need -- and a test
 * that has to build a desktop to assert a sentence is one that stops
 * being written.
 *
 * So an app asks this instead and treats null as "there is nowhere to
 * hand off to", which is exactly what it means. It is deliberately NOT a
 * silent fallback for chrome: `useOs` keeps its throw, and this is a
 * different question with a different answer.
 */
export function useOsIfPresent(): OsContextValue | null {
  return useContext(OsContext);
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
 * First-run document: one desk, the Ask widget resting top-right -- the ONE
 * pre-placed thing a fresh desktop carries -- and Settings pinned so the dock
 * is never empty.
 *
 * THE WIZARD IS NOT SEEDED, AND COULD NOT BE (design record
 * 2026-09-07-core-gate-and-honest-install, D4).
 *
 * This runs in a React state initializer: before the connection exists, before
 * the role ladder lands, before the readiness feed has said anything.
 * `Shell.tsx` passes `access?.role ?? ""`, which in production is "",
 * and `roleAdmits` refuses an unknown role against a requirement -- so the
 * seeded wizard was placed for NOBODY, on every production boot. This header
 * used to say a desk seeded before the ladder lands carries no wizard, as
 * though that were an edge case; it was every desk.
 *
 * Fixing the TIMING would not have been enough either. A seed is a one-time
 * act, and an EXISTING desk never gains a widget: the store adopts the stored
 * document and replaces the seeded surfaces wholesale. So a timing fix would
 * have left every desk made before it without a wizard forever -- the
 * population that most needed one.
 *
 * Presence is DERIVED instead, by `SetupPresence`, from the feed and the
 * ladder, on whatever desk is active. "It comes back on a fresh desk", which
 * this file documented and never implemented, becomes "it comes back on any
 * desk the moment a core stop unsettles".
 *
 * `actorRole` stays a parameter: every caller passes one and the tests read
 * it. What changed is that nothing here gates on it any more, which is what
 * the seeded-roster case asserts for every role.
 */
export function seedDocument(registry: OsRegistry, grid: GridSize, actorRole = ""): OsState {
  void actorRole;
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
    installedPacks: [],
    previewPack: null,
    selectedItemId: null,
  };
}

function stateFromDocument(doc: DesktopDocument): OsState {
  const shell: ShellState = {
    desks: doc.desks.map((d) => ({ ...d, windows: [] })),
    activeDeskId: doc.activeDeskId,
    windows: {},
    focusedWindowId: null,
  };
  return {
    shell,
    surfaces: doc.surfaces,
    dock: doc.dock,
    themePack: doc.themePack,
    installedPacks: doc.installedPacks,
    previewPack: null,
    selectedItemId: null,
  };
}

/**
 * Take on a document that arrived while the shell was running (epic
 * memql#4746).
 *
 * ADOPTING IS NOT LOADING, and using stateFromDocument here would be the
 * bug. That function builds a shell with no windows, because at boot there
 * are none; applied to a running shell it closes everything the person has
 * open -- and it closes it in response to something that happened on a
 * different computer.
 *
 * So two things are kept, and each is kept for its own reason:
 *
 *   WINDOWS, because a window is session state that this document has never
 *   carried (spec D11) -- the arriving desktop is not a statement about them
 *   and cannot be read as one. A window whose desk no longer exists goes,
 *   since there is nowhere to draw it. Initial hydration is the exception:
 *   occupied provisional desks remain until a later remote edit removes them.
 *
 *   THE DESK ON SCREEN, when it still exists, because it is where the person
 *   is looking. Following another machine's paging would move the view under
 *   somebody mid-drag. When the desk is gone -- a cold sign-in, where the
 *   local desk ids are another machine's -- the document's own choice is
 *   taken, which is what lands a new browser where you left off.
 */
export function adoptDocument(s: OsState, doc: DesktopDocument, origin: "hydrate" | "remote" = "remote"): OsState {
  const desks = doc.desks.map((d) => ({
    ...d,
    windows: (s.shell.desks.find((local) => local.id === d.id)?.windows ?? []).filter(
      (id) => !!s.shell.windows[id],
    ),
  }));
  // The first graph read may finish AFTER a callback or user action opened
  // an app on the provisional local desktop. Those windows did not come
  // from persistence and must survive hydration, with their component state
  // and intent intact. Keep only occupied provisional desks; empty ones and
  // all their cached items still give way to the arriving document. A later
  // remote edit retains the usual deleted-desk semantics.
  const retained = origin === "hydrate"
    ? s.shell.desks.filter(d => !desks.some(remote => remote.id === d.id) && d.windows.some(id => !!s.shell.windows[id]))
    : [];
  desks.push(...retained.map(d => ({ ...d, windows: d.windows.filter(id => !!s.shell.windows[id]) })));
  const kept = new Set(desks.flatMap((d) => d.windows));
  const windows = Object.fromEntries(
    Object.entries(s.shell.windows).filter(([id]) => kept.has(id)),
  );
  const focusedWindowId =
    s.shell.focusedWindowId !== null && kept.has(s.shell.focusedWindowId)
      ? s.shell.focusedWindowId
      : null;
  const deskIds = new Set(desks.map((d) => d.id));
  return {
    shell: {
      desks,
      activeDeskId: deskIds.has(s.shell.activeDeskId) ? s.shell.activeDeskId : doc.activeDeskId,
      windows,
      focusedWindowId,
    },
    surfaces: { ...doc.surfaces, ...Object.fromEntries(retained.map(d => [d.id, emptySurface()])) },
    dock: doc.dock,
    themePack: doc.themePack,
    installedPacks: doc.installedPacks,
    // A preview belongs to the pointer hovering a card on THIS machine. An
    // arriving desktop says nothing about it, and dropping it would snap the
    // theme out from under somebody mid-hover.
    previewPack: s.previewPack,
    // Selection survives adoption only while its item does: the arriving
    // desktop is a statement about ITEMS, and a selection of one it no
    // longer carries would highlight nothing.
    selectedItemId:
      s.selectedItemId !== null &&
      Object.values(doc.surfaces).some((surface) => !!surface.items[s.selectedItemId!])
        ? s.selectedItemId
        : null,
  };
}

/**
 * Every item id the desktop is currently using, across every desk -- an item
 * lives on exactly one surface but a new one must avoid all of them, because
 * dragging moves items between desks.
 */
function itemIdsOf(surfaces: Record<DeskId, DeskSurface>): ReadonlySet<string> {
  const ids = new Set<string>();
  for (const surface of Object.values(surfaces)) {
    for (const id of Object.keys(surface.items)) ids.add(id);
  }
  return ids;
}

/** A fresh item id that nothing on this desktop already holds. */
function mintItemId(s: OsState): string {
  return nextIdAvoiding("item", itemIdsOf(s.surfaces));
}

/** How long a "saved on another machine" report stays up. */
const ROAMED_NOTICE_MS = 6_000;

export function OsProvider({
  children,
  registry,
  actorRole,
  ladderLoaded = true,
  accessEpoch = 0,
  store,
  grid,
  layout = "desktop",
}: {
  children: ReactNode;
  registry: OsRegistry;
  actorRole: string;
  /**
   * Defaults TRUE so every existing harness -- which seeds the ladder in
   * test/setup.ts and never renders the pre-load window -- behaves exactly as
   * before. The shell passes the real flag; the pre-load state is false only
   * while the cluster read is in flight.
   */
  ladderLoaded?: boolean;
  /**
   * Defaults 0 for the same reason: a harness installs the effective set in
   * test/setup.ts and never renders the pre-read window. The shell passes
   * the real epoch from the session scope's read.
   */
  accessEpoch?: number;
  store?: DesktopStore;
  grid: GridSize;
  /**
   * Defaults to "desktop" so every existing harness -- none of which renders
   * phone chrome -- behaves exactly as before. The shell passes the real one.
   */
  layout?: ChromeLayout;
}) {
  const storeRef = useRef<DesktopStore>(store ?? new LocalDesktopStore());
  // The store can be REPLACED, once, when the cluster connection arrives and
  // the local store gives way to the graph-backed one (epic memql#4746). Held
  // in a ref, and updated during render exactly as gridRef and actorRoleRef
  // below are, so the actions object built once stays valid.
  if (store && store !== storeRef.current) storeRef.current = store;

  const [state, setState] = useState<OsState>(() => {
    const loaded = storeRef.current.load();
    return loaded ? stateFromDocument(loaded) : seedDocument(registry, grid, actorRole);
  });
  const [notice, setNotice] = useState<OsNotice | null>(null);

  // Persist on every settled change. The document never carries windows,
  // so persisting during interaction is cheap and safe.
  //
  // `store` IS A DEPENDENCY, and not by accident: when the graph-backed store
  // replaces the local one mid-session, this is what hands it the desktop as
  // it stands. Without it a store that arrives after boot holds nothing to
  // reconcile, and a person signing in for the first time would not upload
  // their desktop until they next moved something.
  useEffect(() => {
    storeRef.current.save(
      documentFromState(state.shell, state.surfaces, state.dock, state.themePack, state.installedPacks),
    );
  }, [state, store]);

  // The store's other direction: a desktop this shell did not produce.
  //
  // THE STORE IS THE ONLY DEPENDENCY, and it changes exactly once -- local
  // gives way to graph-backed when the connection dials. Nothing else may be
  // added here: a value that merely arrives late (an actor id resolving, a
  // grid resize) would tear the subscription down and re-register it
  // mid-session, which means dropping the one the cluster is delivering to
  // and re-reading for no reason.
  useEffect(() => {
    const desktopStore = storeRef.current;
    if (!desktopStore.subscribe) return;
    return desktopStore.subscribe((event) => {
      if (event.kind === "stale") {
        setNotice({ kind: "stale" });
        return;
      }
      setState((s) => adoptDocument(s, event.document, event.origin));
      // `hydrate` is the desktop resolving for the first time, which is not
      // news -- it is what the person expected to see. Only a document that
      // arrived AFTER that means somebody else's machine saved.
      if (event.origin === "remote") setNotice({ kind: "roamed" });
    });
  }, [store]);

  // The roamed report clears itself; `stale` stays, because it describes a
  // condition that is still true in a minute.
  useEffect(() => {
    if (notice?.kind !== "roamed") return;
    const timer = setTimeout(() => setNotice(null), ROAMED_NOTICE_MS);
    return () => clearTimeout(timer);
  }, [notice]);

  // A save the store is still holding behind its debounce, sent as the page
  // goes away -- so "arrange a desk, close the laptop, open the other
  // machine" carries the last change rather than the one before it. Local
  // already has it either way; this is only about the cluster.
  useEffect(() => {
    const desktopStore = storeRef.current;
    if (!desktopStore.flush) return;
    const flush = () => desktopStore.flush?.();
    window.addEventListener("pagehide", flush);
    return () => window.removeEventListener("pagehide", flush);
  }, [store]);

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

  // The CURRENT state, readable synchronously from an action. The older
  // "let outcome; set(updater); return outcome" shape only works when React
  // evaluates the queued updater eagerly, which it does exactly when the
  // fiber's queue is empty -- the second call in a session reads a stale
  // "full". An action that must ANSWER (placed / focused / full) computes
  // from here and then applies.
  const stateRef = useRef<OsState | null>(null);
  stateRef.current = state;

  const actionsRef = useRef<OsActions | null>(null);
  if (!actionsRef.current) {
    const set = (updater: (s: OsState) => OsState) => setState((s) => pruneSurfaces(updater(s)));
    let lastEffect: ShellEffect = { kind: "none" };

    actionsRef.current = {
      openApp: (appId, sectionId, payload) => {
        lastEffect = { kind: "none" };
        // Minted OUTSIDE the updater: React may re-run updaters, and an id
        // minted inside would advance the counter once per run.
        const intent = payload ? { id: nextId("intent"), payload } : undefined;
        set((s) => {
          if (!canOpen(registry, appId)) return s;
          const target = sectionId ?? defaultSection(registry, appId);
          const { state: shell, effect } = openAppFn(s.shell, appId, target, intent);
          lastEffect = effect;
          return { ...s, shell };
        });
        return lastEffect;
      },
      consumeWindowIntent: (windowId, intentId) =>
        set((s) => ({ ...s, shell: consumeIntentFn(s.shell, windowId, intentId) })),
      closeWindow: (id) => set((s) => ({ ...s, shell: closeWindowFn(s.shell, id).state })),
      minimizeWindow: (id) => set((s) => ({ ...s, shell: setWindowMode(s.shell, id, "minimized") })),
      toggleFullscreen: (id) =>
        set((s) => ({
          ...s,
          shell: toggleFullscreenFn(s.shell, id, hasContent(s)),
        })),
      focusWindow: (id) => set((s) => ({ ...s, shell: focusWindowFn(s.shell, id) })),
      navigateSection: (id, sectionId, origin) => set((s) => ({ ...s, shell: setWindowSection(s.shell, id, sectionId, origin) })),
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
      selectSurfaceItem: (itemId) => set((s) => ({ ...s, selectedItemId: itemId })),
      sendFileToDesk: (entry) => {
        const s = stateRef.current;
        if (!s) return "full";
        const deskId = s.shell.activeDeskId;
        const surface = surfaceOf(s, deskId);
        // The dedupe rule: an item already on the ACTIVE desk is focused,
        // never duplicated. Matched by artifact -- the desk id is minted
        // per shortcut and means nothing to the Library.
        const existing = Object.values(surface.items).find(
          (i) => i.kind === "file" && entry.artifactId !== "" && i.artifactId === entry.artifactId,
        );
        if (existing) {
          set((cur) => ({ ...cur, selectedItemId: existing.id }));
          return "focused";
        }
        const id = mintItemId(s);
        const placed = addItem(surface, { kind: "file", id, ...entry }, { col: 0, row: 0 }, gridRef.current);
        if (!placed) return "full";
        set((cur) => ({ ...withSurface(cur, deskId, placed), selectedItemId: id }));
        return "placed";
      },
      sendFolderToDesk: (folderId, name) => {
        const s = stateRef.current;
        if (!s) return "full";
        const deskId = s.shell.activeDeskId;
        const surface = surfaceOf(s, deskId);
        const existing = Object.values(surface.items).find(
          (i) => i.kind === "folder" && i.folderId === folderId,
        );
        if (existing) {
          set((cur) => ({ ...cur, selectedItemId: existing.id }));
          return "focused";
        }
        const id = mintItemId(s);
        const placed = addItem(
          surface,
          { kind: "folder", id, folderId, name },
          { col: 0, row: 0 },
          gridRef.current,
        );
        if (!placed) return "full";
        set((cur) => ({ ...withSurface(cur, deskId, placed), selectedItemId: id }));
        return "placed";
      },
      placeFolderShortcut: (shortcut, preferred) => {
        const s = stateRef.current;
        if (!s) return false;
        const deskId = s.shell.activeDeskId;
        const placed = addItem(
          surfaceOf(s, deskId),
          { kind: "folder", id: mintItemId(s), ...shortcut },
          preferred,
          gridRef.current,
        );
        if (!placed) return false;
        set((cur) => withSurface(cur, deskId, placed));
        return true;
      },
      renameFolderShortcut: (itemId, name) =>
        set((s) => {
          const deskId = deskOfItem(s, itemId);
          return deskId ? withSurface(s, deskId, updateFolderFn(surfaceOf(s, deskId), itemId, { name })) : s;
        }),
      addWidget: (widgetId) => {
        let ok = false;
        set((s) => {
          const manifest = widgetById(registry, widgetId);
          if (!manifest) return s;
          // THE ROLE GATE, which this action did not have (found while
          // re-reading every requirement for epic memql#4832, D1).
          //
          // `openApp` above checks `canOpen`; this did not, so a widget
          // carrying `roles` was addable from the DESK CONTEXT MENU by any
          // role -- the launcher's Widgets tab filters through
          // widgetsForRole, and the desk menu offered the unfiltered list.
          // Inert today because the one shipped widget (Ask) declares no
          // role, which is exactly why it went unnoticed: the first gated
          // widget would have shipped the hole with it.
          //
          // Checked HERE rather than only in the menu for the reason openApp
          // is: the menu is one caller, and an action that trusts its callers
          // is an action whose next caller does not know it had to.
          if (!accessAdmits(manifest.requires)) return s;
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
            id: mintItemId(s),
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
      ensureWidget: (widgetId, under) => {
        set((s) => {
          const manifest = widgetById(registry, widgetId);
          if (!manifest) return s;
          // The role gate addWidget states in full, for the reason it states:
          // an action that trusts its callers is an action whose next caller
          // does not know it had to.
          if (!accessAdmits(manifest.requires)) return s;
          const deskId = s.shell.activeDeskId;
          const surface = surfaceOf(s, deskId);
          // `items` is keyed by item id, not a list -- Object.values, the way
          // addWidget reads it.
          const items = Object.values(surface.items);
          if (items.some((i) => i.kind === "widget" && i.widgetId === widgetId)) return s;
          // The anchor's own placement, not its item: a surface keeps items
          // and positions in two maps, so a widget's row lives in the second.
          const anchor = under
            ? items.find((i) => i.kind === "widget" && i.widgetId === under)
            : undefined;
          const anchorAt = anchor ? surface.positions[anchor.id] : undefined;
          const anchorSpan = anchor && anchor.kind === "widget" ? anchor.h : 0;
          const placed = addItem(
            surface,
            { kind: "widget", id: mintItemId(s), widgetId, w: manifest.size.w, h: manifest.size.h },
            // Under its anchor on the same edge, so the two pre-placed things
            // read as one column rather than two unrelated cards. `addItem`
            // settles on the nearest free cell, so a desk too short for this
            // row places it wherever it fits instead of dropping it.
            {
              col: Math.max(0, gridRef.current.cols - manifest.size.w),
              row: anchorAt ? anchorAt.row + anchorSpan : 0,
            },
            gridRef.current,
          );
          return withSurface(s, deskId, placed);
        });
      },
      removeWidget: (itemId) => actionsRef.current!.removeSurfaceItem(itemId),
      sortActiveDesk: () =>
        set((s) => {
          const deskId = s.shell.activeDeskId;
          return withSurface(s, deskId, sortSurface(surfaceOf(s, deskId), gridRef.current));
        }),
      setThemePack: (pack) => set((s) => ({ ...s, themePack: pack, previewPack: null })),
      installThemePack: (pack) =>
        set((s) => ({
          ...s,
          installedPacks: [...s.installedPacks.filter((p) => p.id !== pack.id), pack],
        })),
      removeThemePack: (id) =>
        set((s) => ({
          ...s,
          installedPacks: s.installedPacks.filter((p) => p.id !== id),
          // Uninstalling the pack you are WEARING has to take the desktop
          // somewhere. Graphite is the only answer that always exists: its
          // tokens are the bundle's unqualified :root.
          themePack: s.themePack === id ? BUILT_IN_THEME_ID : s.themePack,
          previewPack: s.previewPack === id ? null : s.previewPack,
        })),
      previewThemePack: (id) => set((s) => (s.previewPack === id ? s : { ...s, previewPack: id })),
    };
  }

  // actorRole can change after a refresh of MyAccess facts; keep the
  // stable actions object reading the current value.
  const actorRoleRef = useRef(actorRole);
  actorRoleRef.current = actorRole;

  const value = useMemo<OsContextValue>(
    () => ({ state, actions: actionsRef.current!, registry, actorRole, ladderLoaded, accessEpoch, grid, layout, notice }),
    [state, registry, actorRole, ladderLoaded, accessEpoch, grid, layout, notice],
  );

  // Every installed pack's CSS, in one style element, kept in step with the
  // list. Before the attribute effect below: a pack must have its rules in
  // the document by the time the root names it, or the first frame of a
  // preview is the previous theme.
  useEffect(() => {
    applyPackStyles(state.installedPacks);
  }, [state.installedPacks]);

  useEffect(() => {
    // The PREVIEW wins while one is open -- that is the marketplace's whole
    // gesture, and it is why this is one attribute write rather than two
    // places that could disagree about which theme is on screen.
    //
    // The stored id is stamped VERBATIM, never resolved: a desktop roamed
    // from a machine with a pack this bundle has not installed yet keeps
    // naming that pack, so it takes effect the moment it arrives instead of
    // being silently rewritten to graphite behind the person's back.
    document.documentElement.setAttribute("data-os-theme", state.previewPack ?? state.themePack);
  }, [state.themePack, state.previewPack]);

  return <OsContext.Provider value={value}>{children}</OsContext.Provider>;
}

function deskOfItem(s: OsState, itemId: string): DeskId | null {
  for (const [deskId, surface] of Object.entries(s.surfaces)) {
    if (surface.items[itemId]) return deskId;
  }
  return null;
}

function defaultSection(registry: OsRegistry, appId: AppId): string {
  const app = registry.apps.find((a) => a.id === appId);
  if (!app?.sections?.length) return "";
  return sectionsFor(app)[0]?.id ?? "";
}

/** GC pass exposed for tests: prune desks then surfaces coherently. */
export function gcState(state: OsState): OsState {
  const shell = gcAutoDesks(state.shell, (deskId) => surfaceHasContent(state.surfaces[deskId]));
  const deskIds = new Set(shell.desks.map((d) => d.id));
  const surfaces = Object.fromEntries(Object.entries(state.surfaces).filter(([id]) => deskIds.has(id)));
  return { ...state, shell, surfaces };
}
