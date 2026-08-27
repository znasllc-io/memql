import { useEffect, useRef, useState } from "react";
import { closestCenter, DndContext, type DragEndEvent } from "@dnd-kit/core";
import { horizontalListSortingStrategy, SortableContext, useSortable } from "@dnd-kit/sortable";
import { LayoutGrid, Sparkles } from "lucide-react";

import { useAsk } from "../ask/AskProvider";
import { dockOrder, isPinned } from "../system/dock";
import { appById, appsForRole, canOpen } from "../system/registry";
import { readStoredTheme, setTheme, type ThemeChoice } from "../app/theme";
import type { AppId } from "../system/windows";
import { useSession } from "./access";
import { useConnectionStatus } from "./connection";
import { ContextMenu, type MenuEntry } from "./ContextMenu";
import { useOs } from "./state";

// The one bar (spec A): Launcher at the left end; pinned then running apps
// center (running dot beneath, right-click to pin/unpin/close, drag to
// reorder pins); right cluster = Ask orb, connection dot, clock, avatar
// menu. Pins persist through the DesktopStore.

function DockApp({
  appId,
  running,
  sortable,
  onLaunch,
  onMenu,
}: {
  appId: AppId;
  running: boolean;
  sortable: boolean;
  onLaunch: () => void;
  onMenu: (event: React.MouseEvent) => void;
}) {
  const { registry } = useOs();
  const manifest = appById(registry, appId);
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({
    id: `pin:${appId}`,
    disabled: !sortable,
  });
  if (!manifest) return null;
  const Icon = manifest.icon;
  return (
    <button
      ref={setNodeRef}
      type="button"
      className="os-dock-app"
      data-running={running || undefined}
      data-dragging={isDragging || undefined}
      style={{
        transform: transform ? `translate(${transform.x}px, ${transform.y}px)` : undefined,
        transition: transition ?? undefined,
      }}
      aria-label={`${manifest.name}${running ? " (running)" : ""}`}
      onClick={onLaunch}
      onContextMenu={onMenu}
      {...attributes}
      {...listeners}
    >
      <Icon size={20} aria-hidden />
      <span className="os-dock-dot" data-on={running || undefined} aria-hidden="true" />
    </button>
  );
}

function Clock() {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const tick = () => setNow(new Date());
    const cleanup = { current: () => {} };
    const align = setTimeout(() => {
      tick();
      const every = setInterval(tick, 60_000);
      cleanup.current = () => clearInterval(every);
    }, (60 - new Date().getSeconds()) * 1000);
    cleanup.current = () => clearTimeout(align);
    return () => cleanup.current();
  }, []);
  const hh = String(now.getHours()).padStart(2, "0");
  const mm = String(now.getMinutes()).padStart(2, "0");
  return (
    <time className="os-clock" dateTime={now.toISOString()}>
      {hh}:{mm}
    </time>
  );
}

function AvatarMenu({ onSignOut }: { onSignOut: () => void }) {
  const { access } = useSession();
  const [open, setOpen] = useState(false);
  const [mode, setMode] = useState<ThemeChoice>(() => readStoredTheme());
  const ref = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!open) return;
    const onPointer = (event: PointerEvent) => {
      if (!ref.current?.contains(event.target as Node)) setOpen(false);
    };
    window.addEventListener("pointerdown", onPointer, true);
    return () => window.removeEventListener("pointerdown", onPointer, true);
  }, [open]);

  const initial = (access?.primaryEmail || "?").slice(0, 1).toUpperCase();

  function choose(choice: ThemeChoice) {
    setMode(choice);
    setTheme(choice);
  }

  return (
    <div className="os-avatar-anchor" ref={ref}>
      <button
        type="button"
        className="os-avatar"
        aria-label="Account menu"
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        {initial}
      </button>
      {open ? (
        <div role="menu" aria-label="Account" className="os-menu os-avatar-menu">
          <p className="os-menu-head">{access?.primaryEmail || "Signed in"}</p>
          <div className="os-menu-modes" role="group" aria-label="Mode">
            {(["light", "dark", "system"] as const).map((choice) => (
              <button
                key={choice}
                type="button"
                role="menuitemradio"
                aria-checked={mode === choice}
                className="os-menu-mode"
                onClick={() => choose(choice)}
              >
                {choice === "light" ? "Light" : choice === "dark" ? "Dark" : "System"}
              </button>
            ))}
          </div>
          <button
            type="button"
            role="menuitem"
            className="os-menu-item"
            onClick={() => {
              setOpen(false);
              onSignOut();
            }}
          >
            Sign out
          </button>
        </div>
      ) : null}
    </div>
  );
}

export function Dock({
  onOpenLauncher,
  onSignOut,
}: {
  onOpenLauncher: () => void;
  onSignOut: () => void;
}) {
  const { state, actions, registry, actorRole } = useOs();
  const { openAsk } = useAsk();
  const connection = useConnectionStatus();
  const [menu, setMenu] = useState<{ x: number; y: number; appId: AppId } | null>(null);

  const runningIds = Object.values(state.shell.windows).map((w) => w.appId);
  const visible = dockOrder(state.dock, runningIds).filter((id) => canOpen(registry, actorRole, id));
  const pinnedVisible = state.dock.pinned.filter((id) =>
    appsForRole(registry, actorRole).some((a) => a.id === id),
  );

  function onDragEnd(event: DragEndEvent) {
    const activeId = String(event.active.id).replace(/^pin:/, "");
    const overId = event.over ? String(event.over.id).replace(/^pin:/, "") : null;
    if (!overId || activeId === overId) return;
    const toIndex = state.dock.pinned.indexOf(overId);
    if (toIndex >= 0) actions.movePin(activeId, toIndex);
  }

  function menuEntries(appId: AppId): MenuEntry[] {
    const win = Object.values(state.shell.windows).find((w) => w.appId === appId);
    return [
      isPinned(state.dock, appId)
        ? { id: "unpin", label: "Unpin from dock", onSelect: () => actions.unpinApp(appId) }
        : { id: "pin", label: "Pin to dock", onSelect: () => actions.pinApp(appId) },
      ...(win
        ? [{ id: "close", label: "Close", onSelect: () => actions.closeWindow(win.id) }]
        : []),
    ];
  }

  return (
    <footer className="os-dock" data-os-dock>
      <button
        type="button"
        className="os-dock-app os-dock-launcher"
        aria-label="Launcher"
        onClick={onOpenLauncher}
      >
        <LayoutGrid size={20} aria-hidden />
      </button>
      <div className="os-dock-strip" role="toolbar" aria-label="Apps">
        <DndContext collisionDetection={closestCenter} onDragEnd={onDragEnd}>
          <SortableContext
            items={pinnedVisible.map((id) => `pin:${id}`)}
            strategy={horizontalListSortingStrategy}
          >
            {visible.map((appId) => (
              <DockApp
                key={appId}
                appId={appId}
                running={runningIds.includes(appId)}
                sortable={isPinned(state.dock, appId)}
                onLaunch={() => actions.openApp(appId)}
                onMenu={(event) => {
                  event.preventDefault();
                  setMenu({ x: event.clientX, y: event.clientY, appId });
                }}
              />
            ))}
          </SortableContext>
        </DndContext>
      </div>
      <div className="os-dock-status">
        <button type="button" className="os-ask-orb" aria-label="Ask" onClick={() => openAsk(null)}>
          <Sparkles size={16} aria-hidden />
        </button>
        <span
          className="os-dot os-connection-dot"
          data-os-dot={connection === "connected" ? "reachable" : connection === "reconnecting" ? "unreachable" : "off"}
          role="img"
          aria-label={`Cluster connection: ${connection}`}
        />
        <Clock />
        <AvatarMenu onSignOut={onSignOut} />
      </div>
      {menu ? (
        <ContextMenu
          x={menu.x}
          y={menu.y}
          label="App"
          entries={menuEntries(menu.appId)}
          onClose={() => setMenu(null)}
        />
      ) : null}
    </footer>
  );
}
