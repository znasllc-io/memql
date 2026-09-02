import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useDroppable, type DragEndEvent } from "@dnd-kit/core";
import { horizontalListSortingStrategy, SortableContext, useSortable } from "@dnd-kit/sortable";
import { LayoutGrid, RefreshCw, Sparkles } from "lucide-react";

import { useAsk } from "../ask/AskProvider";
import { BIN_DROPPABLE_ID, type BinDropPayload } from "../apps/bin/concepts";
import { decideBinDrop } from "../apps/bin/drop";
import { LocalFilesSettingsStore } from "../apps/files/settings";
import { useOsConnection } from "../live/connection";
import { Button, Notice } from "../kit";
import { dockOrder, isPinned } from "../system/dock";
import { appById, appsForRole, canOpen, fixturesForRole, isDockFixture } from "../system/registry";
import { useActiveDrag, useShellDragClaim } from "./dragScope";
import { readStoredTheme, setTheme, type ThemeChoice } from "../app/theme";
import type { OsAppManifest } from "../system/registry";
import type { AppId } from "../system/windows";
import { useSession } from "./access";
import { useConnectionStatus } from "./connection";
import { ContextMenu, type MenuEntry } from "./ContextMenu";
import { useOs, type OsNotice } from "./state";

// The one bar (spec A): Launcher at the left end; pinned then running apps
// center (running dot beneath, right-click to pin/unpin/close, drag to
// reorder pins); right cluster = Ask orb, connection dot, clock, avatar
// menu. Pins persist through the DesktopStore.
//
// The roaming desktop's cue lives in that right cluster too (epic
// memql#4746), beside the connection dot, and DELIBERATELY NOT AS A TOAST:
// this app says so twice in its own source (ask/AskSurface.tsx, "never a
// toast"; ask/sdkTransport.ts, "honest, in-surface, retryable") -- a report
// belongs where the thing it reports on is shown, not floating over whatever
// the person was doing. Both of these describe the cluster's relationship to
// this desktop, which is exactly what the dot beside them describes.

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

// The prefixes the dock answers for. Stable at module scope: it is a
// dependency of the registration effect, and an array rebuilt each render
// would re-register on every one -- with a cleanup that unregisters the dock
// mid-drag.
const PIN_DRAG_PREFIXES = ["pin:", "item:", "artifact:"] as const;

/**
 * A dock fixture: always present, never pinnable, and the one drop target in
 * the dock (memql#4784).
 *
 * THE DROP TARGET OFFERS ITSELF WHEN THE DRAG STARTS, not when the pointer
 * arrives. `useActiveDrag` is what makes that possible -- a person dragging a
 * file has to be able to see where it can go before they have gone there, or
 * the gesture is one they have to already know about.
 *
 * IT ALSO REFUSES FILES FROM THE COMPUTER, out loud. The dock sits over the
 * desk plate, whose own dragover handler turns a dropped host file into an
 * upload and a desktop icon -- so a file dropped here would be UPLOADED, by
 * the surface underneath, at the exact moment somebody meant to throw
 * something away. Both phases are stopped for that reason, and the refusal is
 * rendered rather than silent: a target that visibly accepts a drag and then
 * does nothing reads as broken.
 */
function DockFixture({
  name,
  icon: Icon,
  running,
  onLaunch,
  onMenu,
  onHostFile,
  bubble,
}: {
  name: string;
  icon: OsAppManifest["icon"];
  running: boolean;
  onLaunch: () => void;
  onMenu: (event: React.MouseEvent) => void;
  onHostFile: () => void;
  /** The one thing the Bin has to say right now -- a confirm, a refusal, or
   *  nothing. Hoisted to the Dock so a confirm and a refusal can never appear
   *  in two different corners of the same gesture. */
  bubble: ReactNode;
}) {
  const { setNodeRef, isOver } = useDroppable({ id: BIN_DROPPABLE_ID });
  const activeDrag = useActiveDrag();
  const armed = activeDrag.startsWith("item:") || activeDrag.startsWith("artifact:");

  return (
    <div className="os-dock-fixture">
      <button
        ref={setNodeRef}
        type="button"
        className="os-dock-app os-dock-bin"
        data-running={running || undefined}
        data-armed={armed || undefined}
        data-over={isOver || undefined}
        aria-label={`${name}${running ? " (running)" : ""}`}
        onClick={onLaunch}
        onContextMenu={onMenu}
        onDragOver={(event) => {
          if (!event.dataTransfer.types.includes("Files")) return;
          // Stopped in BOTH phases. Without the dragover half the desk's own
          // handler allows the drop and uploads the file regardless of what
          // this one does.
          event.preventDefault();
          event.stopPropagation();
        }}
        onDrop={(event) => {
          if (!event.dataTransfer.types.includes("Files")) return;
          event.preventDefault();
          event.stopPropagation();
          onHostFile();
        }}
      >
        <Icon size={20} aria-hidden />
        <span className="os-dock-dot" data-on={running || undefined} aria-hidden="true" />
      </button>
      {bubble === null ? null : (
        <div className="os-dock-bubble" role="status">
          {bubble}
        </div>
      )}
    </div>
  );
}

/**
 * The roaming report: another machine saved this desktop, or this tab is too
 * old to save it. Absent the rest of the time -- there is no idle state and
 * no dismiss control, because `roamed` clears itself and `stale` is fixed by
 * the reload it asks for.
 */
function RoamNotice({ notice }: { notice: OsNotice | null }) {
  // The live region is ALWAYS mounted, not conditionally rendered: a
  // role="status" element that appears at the same moment its text does is
  // not reliably announced, because there was no region for the change to
  // happen inside.
  return (
    <span className="os-roam" role="status" data-os-roam={notice?.kind}>
      {notice === null ? null : (
        <>
          <RefreshCw size={12} aria-hidden />
          <span className="os-roam-text">
            {notice.kind === "roamed"
              ? "Desktop updated on another device"
              : "This tab is out of date -- reload to save changes"}
          </span>
        </>
      )}
    </span>
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
  const { state, actions, registry, actorRole, ladderLoaded, notice } = useOs();
  const { openAsk } = useAsk();
  const connection = useConnectionStatus();
  const [menu, setMenu] = useState<{ x: number; y: number; appId: AppId } | null>(null);

  // `ladderLoaded` in the deps for the launcher's reason (memql#4857):
  // fixturesForRole reads the role ladder out of band, so a fixture gated
  // above the pre-load answer would otherwise stay hidden after the ladder
  // lands. The unmemoized filters below recompute on the same re-render.
  const fixtures = useMemo(
    () => fixturesForRole(registry, actorRole),
    [registry, actorRole, ladderLoaded],
  );
  const fixtureIds = useMemo(() => fixtures.map((a) => a.id), [fixtures]);

  const runningIds = Object.values(state.shell.windows).map((w) => w.appId);
  const visible = dockOrder(state.dock, runningIds, fixtureIds).filter((id) =>
    canOpen(registry, actorRole, id),
  );
  const pinnedVisible = state.dock.pinned.filter(
    (id) =>
      !fixtureIds.includes(id) && appsForRole(registry, actorRole).some((a) => a.id === id),
  );

  // --- the drop onto the Bin (memql#4784) ---
  //
  // ARCHIVE, NEVER DELETE. Both gestures the issue names -- the row action in
  // Files and this one -- run `archiveArtifact`, which re-versions the row
  // with archived=true and touches nothing else. There is no hard-delete call
  // anywhere in this app.
  const osConnection = useOsConnection();
  const [pending, setPending] = useState<BinDropPayload | null>(null);
  const [binNote, setBinNote] = useState<{ tone: "info" | "error"; sentence: string; next?: string; detail?: string } | null>(null);
  const [archiving, setArchiving] = useState(false);

  const archive = useCallback(
    async (drop: BinDropPayload) => {
      const query = osConnection?.query ?? null;
      if (query === null) {
        setBinNote({ tone: "error", sentence: "Not connected to the cluster, so nothing was archived." });
        return;
      }
      setArchiving(true);
      try {
        await query.archiveArtifact({ artifactId: drop.artifactId });
        // The desk shortcut goes with it: an icon still pointing at something
        // in the Bin is a shortcut to a thing this desktop says is not here.
        // The Library row is what was archived; this only removes the pointer.
        if (drop.deskItemId !== "") actions.removeSurfaceItem(drop.deskItemId);
        setBinNote({
          tone: "info",
          sentence: `"${drop.name}" is in the Bin.`,
          next: "Nothing was deleted -- open the Bin to look at it or put it back.",
        });
      } catch (err: unknown) {
        setBinNote({
          tone: "error",
          sentence: "The archive was refused.",
          next: `"${drop.name}" is where it was.`,
          detail: err instanceof Error ? err.message : String(err),
        });
      } finally {
        setArchiving(false);
      }
    },
    [osConnection, actions],
  );

  // The dock's whole share of the shell's one drag context: pins reorder,
  // and anything dropped ON the Bin is archived. The desk claims `item:` too
  // and returns early for this droppable, so the two do not collide.
  const onDragEnd = useCallback(
    (event: DragEndEvent) => {
      const rawId = String(event.active.id);
      const overId = event.over ? String(event.over.id) : null;

      // The SAME setting the row action consults, read at drop time rather
      // than held: somebody who just turned it on in another window means it
      // for the next thing they drag.
      const outcome = decideBinDrop(
        overId,
        (event.active.data.current ?? null) as BinDropPayload | null,
        new LocalFilesSettingsStore().load().confirmBeforeArchive,
      );
      switch (outcome.kind) {
        case "refuseFolder":
          setBinNote({
            tone: "info",
            sentence: `"${outcome.name}" is a folder, and the Bin cannot take a whole folder from here.`,
            next: "Archive it from Files, where the confirm can tell you how many things are inside it first.",
          });
          return;
        case "confirm":
          setBinNote(null);
          setPending(outcome.drop);
          return;
        case "archive":
          setBinNote(null);
          void archive(outcome.drop);
          return;
        case "ignore":
          break;
      }
      if (overId === BIN_DROPPABLE_ID) return;

      const activeId = rawId.replace(/^pin:/, "");
      const overPin = overId ? overId.replace(/^pin:/, "") : null;
      if (!overPin || activeId === overPin) return;
      const toIndex = state.dock.pinned.indexOf(overPin);
      if (toIndex >= 0) actions.movePin(activeId, toIndex);
    },
    [state.dock.pinned, actions, archive],
  );
  useShellDragClaim(PIN_DRAG_PREFIXES, { onDragEnd });

  // The report clears itself; the CONFIRM does not, because a question that
  // disappeared while somebody was reading it is a question they now have to
  // ask again.
  useEffect(() => {
    if (binNote === null) return;
    const t = setTimeout(() => setBinNote(null), binNote.tone === "error" ? 12000 : 6000);
    return () => clearTimeout(t);
  }, [binNote]);

  const binBubble: ReactNode =
    pending !== null ? (
      <Notice
        tone="warn"
        sentence={`Archive "${pending.name}"?`}
        next="It goes to the Bin and keeps its bytes, its history and its provenance. Nothing is deleted."
      >
        <div className="os-dock-bubble-actions">
          <Button
            tone="danger"
            busy={archiving}
            busyLabel="Archiving"
            onClick={() => {
              const drop = pending;
              setPending(null);
              void archive(drop);
            }}
          >
            Archive
          </Button>
          <Button onClick={() => setPending(null)}>Cancel</Button>
        </div>
      </Notice>
    ) : binNote !== null ? (
      <Notice tone={binNote.tone} sentence={binNote.sentence} next={binNote.next} detail={binNote.detail} />
    ) : null;

  function menuEntries(appId: AppId): MenuEntry[] {
    const win = Object.values(state.shell.windows).find((w) => w.appId === appId);
    // A FIXTURE OFFERS NO PIN CONTROL AT ALL, rather than a disabled one. It
    // is always here and cannot be otherwise, so both entries would be
    // controls that do nothing -- and "nothing happens where nothing is
    // offered" is the rule this shell's right-click section already states.
    const pinEntry: MenuEntry[] = isDockFixture(registry, appId)
      ? []
      : [
          isPinned(state.dock, appId)
            ? { id: "unpin", label: "Unpin from dock", onSelect: () => actions.unpinApp(appId) }
            : { id: "pin", label: "Pin to dock", onSelect: () => actions.pinApp(appId) },
        ];
    return [
      ...pinEntry,
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
      </div>

      {/* The fixtures, in their own slot at the trailing end of the strip --
          the place a desktop trash can has lived for forty years. Separated by
          the strip's own border rather than a rule of its own: it is the same
          divider the launcher already sits behind. */}
      {fixtures.map((app) => (
        <DockFixture
          key={app.id}
          name={app.name}
          icon={app.icon}
          running={runningIds.includes(app.id)}
          onLaunch={() => actions.openApp(app.id)}
          onMenu={(event) => {
            event.preventDefault();
            setMenu({ x: event.clientX, y: event.clientY, appId: app.id });
          }}
          onHostFile={() =>
            setBinNote({
              tone: "info",
              sentence: "The Bin does not take files from your computer.",
              next: "Drop it on the desktop to add it to your Library, or drag something already in MemQL here to archive it.",
            })
          }
          bubble={binBubble}
        />
      ))}
      <div className="os-dock-status">
        <RoamNotice notice={notice} />
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
