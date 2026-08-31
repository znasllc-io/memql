import { useEffect, useMemo, useState, type ReactNode } from "react";

import type { ChromeLayout } from "../app/layout";
import { AskProvider } from "../ask/AskProvider";
import { AskSheet } from "../ask/AskSheet";
import { SdkAskTransport } from "../ask/sdkTransport";
import { type AskTransport } from "../ask/askController";
import { OS_REGISTRY } from "../apps/registry";
import type { OsAuthSource } from "../auth/source";
import type { OsRuntimeConfig } from "../cluster/config";
import { EdgeUploadProvider } from "../items/edgeUpload";
import { type UploadProvider } from "../items/upload";
import { SdkDesktopGateway } from "../live/desktopGateway";
import { MachinesProvider } from "../live/machines";
import { OsConnectionProvider, useOsConnection } from "../live/connection";
import type { ProfileAccess } from "../modules/profile/access";
import type { PlacementTokens } from "../system/placement";
import { GraphDesktopStore } from "../system/graphStore";
import { LocalDesktopStore, type DesktopStore } from "../system/store";
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
  /** Tests: skip dialing the cluster entirely. */
  disableConnection?: boolean;
}

export function Shell({
  layout,
  onSignOut,
  access = null,
  config,
  authSource,
  ports = {},
}: {
  layout: ChromeLayout;
  onSignOut: () => void;
  access?: ProfileAccess | null;
  config: OsRuntimeConfig;
  /** The credential seam; absent = dial and fetch with nothing. */
  authSource?: OsAuthSource;
  ports?: ShellPorts;
}) {
  const actorRole = access?.clusterRole ?? "";
  const source = useMemo<OsAuthSource>(
    () => authSource ?? { bearer: async () => null, refresh: async () => null },
    [authSource],
  );

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
      <OsConnectionProvider authSource={source} enabled={!ports.disableConnection}>
        <MachinesProvider>
          <ShellTransports source={source} ports={ports}>
            {(uploads, desktopStore) => (
              <OsProvider registry={OS_REGISTRY} actorRole={actorRole} grid={grid} store={desktopStore}>
                {layout === "phone" ? (
                  <div className="os-root" data-os-root data-layout={layout}>
                    <PhoneShell onSignOut={onSignOut} />
                    <AskSheet />
                  </div>
                ) : (
                  <DesktopChrome
                    layout={layout}
                    viewport={viewport}
                    uploads={uploads}
                    onSignOut={onSignOut}
                  />
                )}
              </OsProvider>
            )}
          </ShellTransports>
        </MachinesProvider>
      </OsConnectionProvider>
    </SessionProvider>
  );
}

// The default transports need the live connection, so they compose INSIDE
// the connection provider; injected ports (tests, previews) still win.
function ShellTransports({
  source,
  ports,
  children,
}: {
  source: OsAuthSource;
  ports: ShellPorts;
  children: (uploads: UploadProvider, store: DesktopStore) => ReactNode;
}) {
  const connection = useOsConnection();
  const askTransport = useMemo<AskTransport>(
    () => ports.askTransport ?? new SdkAskTransport(() => connection?.dispatcher ?? null),
    [ports.askTransport, connection],
  );
  const uploads = useMemo<UploadProvider>(
    () => ports.uploads ?? new EdgeUploadProvider(() => source.bearer()),
    [ports.uploads, source],
  );
  // The desktop store, which is the local one until there is a cluster to
  // roam to (epic memql#4746).
  //
  // IT IS BUILT FROM THE CONNECTION rather than given a getter for it, unlike
  // the Ask transport above, because it SUBSCRIBES: `watch` has to be handed
  // a live SubscriptionManager at the moment it registers, and a getter that
  // was null then would leave a store that reads and writes but never hears
  // another machine -- working, and silently half a feature.
  //
  // The identity therefore changes exactly once in a session, null -> dialed.
  // The SDK owns reconnect (memql#4537: it redials and replays subscriptions
  // on the same Connection), so a dropped socket does NOT rebuild this and
  // does not restart the resolve-then-write sequence.
  const desktopStore = useMemo<DesktopStore>(() => {
    if (ports.store) return ports.store;
    if (connection === null) return new LocalDesktopStore();
    return new GraphDesktopStore(new LocalDesktopStore(), new SdkDesktopGateway(connection));
  }, [ports.store, connection]);
  return <AskProvider transport={askTransport}>{children(uploads, desktopStore)}</AskProvider>;
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
