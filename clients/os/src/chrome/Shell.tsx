import { useEffect, useMemo, useState } from "react";

import type { ChromeLayout } from "../app/layout";
import { AskProvider } from "../ask/AskProvider";
import { AskSheet } from "../ask/AskSheet";
import { StubAskTransport, type AskTransport } from "../ask/askController";
import { OS_REGISTRY } from "../apps/registry";
import type { OsRuntimeConfig } from "../cluster/config";
import { InMemoryUploadProvider, type UploadProvider } from "../items/upload";
import type { ProfileAccess } from "../modules/profile/access";
import type { PlacementTokens } from "../system/placement";
import type { DesktopStore } from "../system/store";
import { SessionProvider } from "./access";
import { Desktop } from "./Desktop";
import { Dock } from "./Dock";
import { gridForViewport, OsProvider, useOs } from "./state";
import { LauncherOverlay } from "./LauncherOverlay";
import { PhoneShell } from "./PhoneShell";

// The shell (spec A): providers + the layout split. Desktop and iPad get
// the desk world; the phone gets its own chrome. Transports and stores
// are injectable so tests and PR B swap them without touching chrome.

export interface ShellPorts {
  askTransport?: AskTransport;
  uploads?: UploadProvider;
  store?: DesktopStore;
}

export function Shell({
  layout,
  onSignOut,
  access = null,
  config,
  ports = {},
}: {
  layout: ChromeLayout;
  onSignOut: () => void;
  access?: ProfileAccess | null;
  config: OsRuntimeConfig;
  ports?: ShellPorts;
}) {
  const askTransport = useMemo(() => ports.askTransport ?? new StubAskTransport(), [ports.askTransport]);
  const uploads = useMemo(() => ports.uploads ?? new InMemoryUploadProvider(), [ports.uploads]);
  const actorRole = access?.clusterRole ?? "";

  const [viewport, setViewport] = useState(() => ({
    w: window.innerWidth,
    h: window.innerHeight,
  }));
  useEffect(() => {
    const onResize = () => setViewport({ w: window.innerWidth, h: window.innerHeight });
    window.addEventListener("resize", onResize);
    return () => window.removeEventListener("resize", onResize);
  }, []);

  const grid = useMemo(() => gridForViewport(viewport.w, viewport.h), [viewport]);

  return (
    <SessionProvider value={{ access, config }}>
      <AskProvider transport={askTransport}>
        <OsProvider registry={OS_REGISTRY} actorRole={actorRole} grid={grid} store={ports.store}>
          {layout === "phone" ? (
            <div className="os-root" data-os-root data-layout={layout}>
              <PhoneShell onSignOut={onSignOut} />
              <AskSheet />
            </div>
          ) : (
            <DesktopChrome layout={layout} viewport={viewport} uploads={uploads} onSignOut={onSignOut} />
          )}
        </OsProvider>
      </AskProvider>
    </SessionProvider>
  );
}

function DesktopChrome({
  layout,
  viewport,
  uploads,
  onSignOut,
}: {
  layout: ChromeLayout;
  viewport: { w: number; h: number };
  uploads: UploadProvider;
  onSignOut: () => void;
}) {
  const { actions, state } = useOs();
  const [launcherOpen, setLauncherOpen] = useState(false);

  const placement = useMemo<PlacementTokens>(
    () => ({ margin: 28, gutter: 16, dockReserve: 118, maxSoloWidth: 1280 }),
    [],
  );

  // Desk keyboard bindings (spec A): scoped off editable targets and off
  // window content. The launcher toggle works everywhere.
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (!(event.ctrlKey && event.shiftKey)) return;
      if (event.key === " " || event.code === "Space") {
        event.preventDefault();
        setLauncherOpen((v) => !v);
        return;
      }
      const target = event.target instanceof HTMLElement ? event.target : null;
      if (target?.closest("input, textarea, [contenteditable], [data-os-window-content]")) return;
      if (event.key === "ArrowLeft") {
        event.preventDefault();
        actions.switchDeskBy(-1);
      } else if (event.key === "ArrowRight") {
        event.preventDefault();
        actions.switchDeskBy(1);
      } else if (/^[1-9]$/.test(event.key)) {
        const desk = state.shell.desks[Number(event.key) - 1];
        if (desk) {
          event.preventDefault();
          actions.switchDesk(desk.id);
        }
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [actions, state.shell.desks]);

  return (
    <div className="os-root" data-os-root data-layout={layout}>
      <Desktop viewport={viewport} placement={placement} uploads={uploads} />
      <Dock onOpenLauncher={() => setLauncherOpen(true)} onSignOut={onSignOut} />
      <LauncherOverlay open={launcherOpen} onClose={() => setLauncherOpen(false)} />
      <AskSheet />
    </div>
  );
}
