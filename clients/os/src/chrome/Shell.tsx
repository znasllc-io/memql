import { useEffect, useMemo, useState, type ReactNode } from "react";

import type { ChromeLayout } from "../app/layout";
import { AskProvider } from "../ask/AskProvider";
import { ThemeStore } from "../themes/ThemeStore";
import { openMicrophone } from "../ask/micCapture";
import { SdkTranscriber } from "../ask/sdkVoice";
import type { VoicePorts } from "../ask/voiceSession";
import { AskSheet } from "../ask/AskSheet";
import { SdkAskTransport } from "../ask/sdkTransport";
import { type AskTransport } from "../ask/askController";
import { OS_REGISTRY } from "../apps/registry";
import { AuthSourceProvider } from "../auth/context";
import type { OsAuthSource } from "../auth/source";
import type { OsRuntimeConfig } from "../cluster/config";
import { EdgeUploadProvider } from "../items/edgeUpload";
import { type UploadProvider } from "../items/upload";
import { SdkDesktopGateway } from "../live/desktopGateway";
import { MachinesProvider } from "../live/machines";
import { OsConnectionProvider, useOsConnection } from "../live/connection";
import type { ProfileAccess } from "../modules/profile/access";
import { useResolvedAccess } from "../modules/profile/useResolvedAccess";
import type { PlacementTokens } from "../system/placement";
import { GraphDesktopStore } from "../system/graphStore";
import { LocalDesktopStore, type DesktopStore } from "../system/store";
import { SessionProvider, useSession } from "./access";
import { Desktop } from "./Desktop";
import { suppressBrowserMenu } from "./browserMenu";
import { Dock } from "./Dock";
import { gridForViewport, OsProvider, useOs } from "./state";
import { LauncherOverlay } from "./LauncherOverlay";
import { PhoneShell } from "./PhoneShell";

// The shell (spec A): providers + the layout split. Desktop and iPad get
// the desk world; the phone gets its own chrome. Transports and stores
// are injectable so tests and PR B swap them without touching chrome.

export interface ShellPorts {
  askTransport?: AskTransport;
  /**
   * Ask's voice wiring. Absent = the shell builds the browser one; `null` =
   * this window deliberately has none, and the mic control says so. jsdom has
   * no audio stack, so the suite's harnesses pass null rather than leaving
   * every Ask test one keystroke away from calling getUserMedia.
   */
  askVoice?: VoicePorts | null;
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

  // ===========================================================================
  // SESSION RESOLVES *INSIDE* THE CONNECTION, AND THAT ORDER IS THE FIX
  // ===========================================================================
  // Who is signed in comes from `query.getMyAccess()` on the shell's own
  // stream, so the provider that supplies it has to sit BELOW the one that
  // dials. It used to sit above, which is why the facts were fetched over HTTP
  // instead -- from a route that does not exist, silently yielding "no role"
  // to everyone forever (see `useResolvedAccess`).
  //
  // `access` stays a prop and still WINS when given: every test harness and
  // preview passes one, and a caller that already knows is not asking.
  return (
    <AuthSourceProvider source={source}>
      {/* The credential seam, reachable from inside an app. An app that
          uploads somewhere the shell's own provider does not point at -- the
          Training app posts to the space attachment route rather than to the
          Library -- builds its own provider from this, and gets `bearer()`
          rather than the token. */}
      <OsConnectionProvider authSource={source} enabled={!ports.disableConnection}>
        <SessionScope access={access} config={config}>
        <MachinesProvider>
          <ShellTransports source={source} ports={ports}>
            {(uploads, desktopStore) => (
              <ShellRoster grid={grid} store={desktopStore}>
                {layout === "phone" ? (
                  <div
      className="os-root"
      data-os-root
      data-layout={layout}
      onContextMenu={suppressBrowserMenu}
    >
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
              </ShellRoster>
            )}
          </ShellTransports>
        </MachinesProvider>
        </SessionScope>
      </OsConnectionProvider>
    </AuthSourceProvider>
  );
}

/**
 * Resolves the session against the live connection and provides it.
 *
 * A component rather than a hook call in `Shell`, because the read needs the
 * Connection and `Shell` renders the provider that creates it -- a hook there
 * would run one level too high and always see null.
 */
function SessionScope({
  access,
  config,
  children,
}: {
  access: ProfileAccess | null;
  config: OsRuntimeConfig;
  children: ReactNode;
}) {
  const resolved = useResolvedAccess(access);
  const value = useMemo(() => ({ access: resolved, config }), [resolved, config]);
  return <SessionProvider value={value}>{children}</SessionProvider>;
}

/**
 * The app roster, gated by the RESOLVED role.
 *
 * Split out for the same reason `SessionScope` is: `actorRole` used to be read
 * off `Shell`'s prop, which is exactly the value that was always null in
 * production. Reading it from context here means the launcher, the dock and
 * open-by-id all see the role the cluster actually reported.
 */
function ShellRoster({
  grid,
  store,
  children,
}: {
  grid: ReturnType<typeof gridForViewport>;
  store: DesktopStore;
  children: ReactNode;
}) {
  const { access } = useSession();
  return (
    <OsProvider
      registry={OS_REGISTRY}
      actorRole={access?.clusterRole ?? ""}
      grid={grid}
      store={store}
    >
      {children}
    </OsProvider>
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
  // Voice's two halves: the microphone (pure browser) and the transcription
  // wire (this shell's one connection). Built here for the same reason the
  // chat transport is -- one Ask, three entry points, and exactly one place
  // that knows how to reach the cluster.
  const askVoice = useMemo<VoicePorts | null>(() => {
    if (ports.askVoice !== undefined) return ports.askVoice;
    return {
      openMicrophone: () => openMicrophone(),
      transcriber: new SdkTranscriber(() => connection?.dispatcher ?? null),
    };
  }, [ports.askVoice, connection]);
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
  return (
    <AskProvider transport={askTransport} voice={askVoice}>
      {children(uploads, desktopStore)}
    </AskProvider>
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
  // Themes is chrome, like Ask -- see themes/ThemeStore.tsx. Opening it
  // CLOSES the Launcher, because the Launcher is a full-screen glass overlay
  // and the marketplace's whole gesture is watching this desktop restyle.
  const [themesOpen, setThemesOpen] = useState(false);

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
    <div
      className="os-root"
      data-os-root
      data-layout={layout}
      onContextMenu={suppressBrowserMenu}
    >
      <Desktop viewport={viewport} placement={placement} uploads={uploads} />
      <Dock onOpenLauncher={() => setLauncherOpen(true)} onSignOut={onSignOut} />
      <LauncherOverlay
        open={launcherOpen}
        onClose={() => setLauncherOpen(false)}
        onOpenThemes={() => {
          setLauncherOpen(false);
          setThemesOpen(true);
        }}
      />
      <ThemeStore open={themesOpen} onClose={() => setThemesOpen(false)} />
      <AskSheet />
    </div>
  );
}
