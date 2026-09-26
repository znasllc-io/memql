import { AttentionMarker, AttentionDestination } from "../attention/Attention";
import { visiblePageContext, visiblePageLabel } from "../kit/pageContext";
import { PageNavigationProvider } from "../kit/pageNavigation";
import { TrailRow } from "../kit/TrailRow";
import { WindowSearchContext, useWindowSearchHost } from "../kit/windowSearch";
import { useDraggable } from "@dnd-kit/core";
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { X, Search, ListFilter, Maximize2, Minimize2, Minus, Settings2 } from "lucide-react";

import { useDrivingCue } from "../ask/useDrivingCue";
import { useAsk } from "../ask/AskProvider";
import { ProvenanceDot } from "../kit";
import { SurfaceRefused } from "../kit/RankStates";
import {
  canConfigure,
  gateFor,
  markToneFor,
  SurfaceUnconfigured,
} from "../kit/ReadinessStates";
import { MODULE_DESCRIPTIONS } from "../system/modules";
import type { Rect } from "../system/placement";
import {
  allRequirementsFor,
  accessAdmits,
  appsFor,
  requirementsFor,
  sectionsFor,
  type OsAppManifest,
} from "../system/registry";
import type { OsWindow } from "../system/windows";
import { useSession } from "./access";
import { useAppSetupMark } from "./useAppSetupMark";
import { Mark } from "./Mark";
import { useOs } from "./state";
import { WindowErrorBoundary } from "./WindowErrorBoundary";
import { ContextMenu } from "./ContextMenu";

// The window (spec A): glass frame on a token-carrying root, computed rect
// (the desk animates BETWEEN rects; during a drag dnd-kit's transform
// rides on top and transitions pause), title bar with the app identity,
// section breadcrumb, Ask-in-context, settings gear, min / full / close.
// Apps never open windows; they navigate sections inside this one.

export function WindowFrame({
  win,
  manifest,
  rect,
  focused,
  actorRole,
  hidden = false,
  deskId,
}: {
  win: OsWindow;
  manifest: OsAppManifest;
  rect: Rect;
  focused: boolean;
  actorRole: string;
  hidden?: boolean;
  deskId?: string;
}) {
  const { actions, state, registry } = useOs();
  const [menu, setMenu] = useState<{ x: number; y: number } | null>(null);
  const searchDialog = useRef<HTMLDialogElement>(null);
  const [searchQuery, setSearchQuery] = useState("");
  const { host: searchHost, openVisible: openContentSearch } = useWindowSearchHost();
  const [pendingSearch, setPendingSearch] = useState(false);
  useEffect(() => {
    if (!pendingSearch) return;
    // The app may reveal a retained list in its layout effect after navigation.
    const frame = requestAnimationFrame(() => {
      if (!openContentSearch()) { setSearchQuery(""); searchDialog.current?.showModal(); }
      setPendingSearch(false);
    });
    return () => cancelAnimationFrame(frame);
  }, [pendingSearch, win.sectionId, win.sectionNavigation?.revision]);
  const searchApp = () => {
    if (openContentSearch()) return;
    if (manifest.searchSection) {
      actions.navigateSection(win.id, manifest.searchSection);
      setPendingSearch(true);
    } else { setSearchQuery(""); searchDialog.current?.showModal(); }
  };
  const content = useRef<HTMLDivElement>(null);
  const scroll = useRef(new Map<string, number>());
  useEffect(() => {
    if (!hidden) return;
    setMenu(null);
    if (searchDialog.current?.open) searchDialog.current.close();
    content.current?.querySelectorAll<HTMLDialogElement>("dialog[open]").forEach(dialog => dialog.close());
  }, [hidden]);
  useLayoutEffect(() => {
    const node = content.current;
    if (node) node.scrollTop = scroll.current.get(win.sectionId) ?? 0;
    return () => { if (node) scroll.current.set(win.sectionId, node.scrollTop); };
  }, [win.sectionId]);
  const { openAsk } = useAsk();
  const askActivity = useDrivingCue(manifest.id);
  const driving = askActivity ? askActivity.phase === "completed" ? "completed" : "running" : undefined;
  const { attributes, listeners, setNodeRef, transform, isDragging } = useDraggable({
    id: `window:${win.id}`,
  });

  const sections = sectionsFor(manifest);
  const current = sections.find((s) => s.id === win.sectionId) ?? sections[0];
  const destinations = sections.filter(s => !s.parent && s.id !== "settings" && s.id !== "logs");
  const logs = sections.find(s => s.id === "logs");
  const ownDesk = state.shell.desks.find(d => d.windows.includes(win.id));

  // READINESS (design record 2026-09-06-configuration-readiness, 5.3 to 5.5).
  //
  // TWO gates, because they answer different questions. `gate` is about the
  // section on screen and decides whether its body renders. `appGate` is about
  // the whole app and decides the mark on Settings -- which must stay lit
  // while the person is standing in Settings fixing it, so it cannot come from
  // the section gate (requirementsFor returns nothing for Settings by design).
  const { readiness } = useSession();
  const sectionReqs = requirementsFor(manifest, current?.id ?? "");
  const gate = gateFor(readiness, sectionReqs.requires, sectionReqs.wants);
  const appReqs = allRequirementsFor(manifest);
  const appGate = gateFor(readiness, appReqs.requires, appReqs.wants);
  const { settingsTone, reportSetupState } = useAppSetupMark(manifest.id, markToneFor(appGate));
  // The dot inside a button is DECORATIVE and the button says the state
  // itself: a labelled role="img" nested in a button appends to the button's
  // accessible name, so "Settings" would announce as "Settings Campaigns is
  // not set up" -- the right information in a shape nothing can match on.
  const settingsStatePhrase = settingsTone === "needsSetup" ? "not set up" : "partly set up";
  // A module the SECTION alone asked for names the section in the headline
  // ("Workbenches is not set up yet"); an app-level one names the app.
  const appRequires = manifest.requires ?? [];
  const unmetIsSectionOnly = gate.unmet.some((id) => !appRequires.includes(id));
  const Icon = manifest.icon;
  const Body = manifest.component;
  const contextTag = `app:${manifest.id}${current ? ` section:${current.id}` : ""}`;

  const style: React.CSSProperties = {
    left: rect.x,
    top: rect.y,
    width: rect.w,
    height: rect.h,
    transform: transform ? `translate(${transform.x}px, ${transform.y}px)` : undefined,
  };

  return (
    <section
      ref={setNodeRef}
      className="os-window"
      data-os-window={manifest.id}
      data-ask-driving={driving}
      data-os-window-desk={deskId}
      hidden={hidden}
      data-focused={focused || undefined}
      data-fullscreen={win.mode === "fullscreen" || undefined}
      data-dragging={isDragging || undefined}
      style={style}
      role="dialog"
      aria-label={manifest.name}
      onPointerDown={() => actions.focusWindow(win.id)}
    >
      <header className="os-window-bar">
        <button
          type="button"
          className="os-window-grip"
          aria-label={`Move ${manifest.name} -- drag or press Shift+F10 for desktop choices`}
          {...listeners}
          {...attributes}
          onContextMenu={event => { event.preventDefault(); setMenu({ x: event.clientX, y: event.clientY }); }}
          onKeyDown={event => { if (event.key === "F10" && event.shiftKey) { event.preventDefault(); const box = event.currentTarget.getBoundingClientRect(); setMenu({ x: box.left, y: box.bottom }); } else listeners?.onKeyDown?.(event); }}
        >
          <Icon size={14} aria-hidden /><AttentionMarker appId={manifest.id} />

        </button>
        <select className="os-window-app-selector" aria-label={`Switch app from ${manifest.name}`} value={manifest.id} onChange={event => {
          const existing = Object.values(state.shell.windows).find(window => window.appId === event.target.value);
          if (existing) actions.focusWindow(existing.id);
          else actions.openApp(event.target.value);
        }}>
          {appsFor(registry).map(app => <option key={app.id} value={app.id}>{app.name}</option>)}
        </select>
        <div className="os-window-drag-space" {...listeners} {...attributes} aria-label={`Drag ${manifest.name}`} />
        <div className="os-window-controls">
          <div className="os-window-control-group">
          <button
            type="button"
            className="os-icon-button"
            aria-label={`Ask about ${manifest.name}`}
            title={`Ask about ${manifest.name}`}
            onClick={() => openAsk(visiblePageContext(content.current, contextTag), visiblePageLabel(content.current, `${manifest.name} / ${current?.name ?? ""}`))}
          >
            <Mark size={14} aria-hidden />
          </button>
          <button type="button" className="os-icon-button" aria-label={`Search ${manifest.name}`} title={`Search ${manifest.name}`} onClick={searchApp}><Search size={14} aria-hidden /></button>
          </div><div className="os-window-control-group">
          {manifest.settingsSection ? (
            <button
              type="button"
              className="os-icon-button"
              data-os-setup={settingsTone ? "" : undefined}
              aria-label={
                settingsTone
                  ? `${manifest.name} settings, ${settingsStatePhrase}`
                  : `${manifest.name} settings`
              }
              aria-current={current?.id === manifest.settingsSection ? "page" : undefined}
              title={`${manifest.name} settings`}
              onClick={() => actions.navigateSection(win.id, manifest.settingsSection!)}
            >
              <Settings2 size={14} aria-hidden /><AttentionMarker appId={manifest.id} sectionId={manifest.settingsSection} />
              {settingsTone ? <ProvenanceDot tone={settingsTone} /> : null}
            </button>
          ) : null}
          {logs ? <button type="button" className="os-icon-button" aria-label={`${manifest.name} logs`} title={`${manifest.name} logs`} onClick={() => actions.navigateSection(win.id, logs.id)}><ListFilter size={14} aria-hidden /><AttentionMarker appId={manifest.id} sectionId={logs.id} /></button> : null}
          </div><div className="os-window-control-group">
          <button
            type="button"
            className="os-icon-button"
            aria-label={`Minimize ${manifest.name}`}
            title={`Minimize ${manifest.name}`}
            onClick={() => actions.minimizeWindow(win.id)}
          >
            <Minus size={14} aria-hidden />
          </button>
          <button
            type="button"
            className="os-icon-button"
            aria-label={
              win.mode === "fullscreen" ? `Restore ${manifest.name} to previous desktop` : `Maximize ${manifest.name} on its own desktop`
            }
            title={win.mode === "fullscreen" ? `Restore ${manifest.name}` : `Maximize ${manifest.name} on its own desktop`}
            onClick={() => actions.toggleFullscreen(win.id)}
          >
            {win.mode === "fullscreen" ? <Minimize2 size={14} aria-hidden /> : <Maximize2 size={14} aria-hidden />}
          </button>
          <button type="button" className="os-icon-button os-window-close" aria-label={`Close ${manifest.name}`} title={`Close ${manifest.name}`} onClick={() => actions.closeWindow(win.id)}><X size={14} aria-hidden /></button>
          </div>
        </div>
      </header>
      <dialog ref={searchDialog} className="os-window-search" aria-label={`Search ${manifest.name} destinations`}>
        <header><strong>{manifest.name} destinations</strong><button type="button" className="os-icon-button" aria-label="Close destination search" onClick={() => searchDialog.current?.close()}><X size={14} aria-hidden /></button></header>
        <input autoFocus className="os-input" aria-label="Find a destination" placeholder="Find a destination…" value={searchQuery} onChange={event => setSearchQuery(event.target.value)} />
        <nav aria-label="Matching destinations">{sections.filter(section => section.name.toLowerCase().includes(searchQuery.toLowerCase())).map(section => <button type="button" className="os-window-nav-item" key={section.id} onClick={() => { searchDialog.current?.close(); actions.navigateSection(win.id, section.id, "content"); }}>{section.name}<AttentionMarker appId={manifest.id} sectionId={section.id} /></button>)}</nav>
        {!sections.some(section => section.name.toLowerCase().includes(searchQuery.toLowerCase())) ? <p>No destinations match.</p> : null}
      </dialog>
      <div className="os-window-body">
        {/* THE ONE TRAIL ROW sits UNDER the section tabs, in the same place in
            every window (it is drawn after the nav below). The order is the
            hierarchy: the tabs choose the section, the trail is depth within
            it. Drawn above them it read as though "Deployables > MemQL OS"
            outranked the Deployables tab that is that crumb's own parent.
            The provider spans both so the row can hear the heading that
            publishes from inside the app body; it renders no element of its
            own, so the body's column layout is what it was plus a row. */}
        <PageNavigationProvider root={content} trail={(win.sectionTrail ?? []).map(id => ({ label: sections.find(section => section.id === id)?.name ?? id, onSelect: () => actions.navigateSection(win.id, id, "back") }))}>
        {destinations.length > 1 ? (
          <nav className="os-window-nav" aria-label={`${manifest.name} sections`}>
            {destinations.map((section, index) => (
              <button
                key={section.id}
                type="button"
                className={`os-window-nav-item${index > 1 ? " os-window-secondary-nav" : ""}`}
                data-ask-control={askActivity && section.id === (current?.parent ?? current?.id) ? "running" : undefined}
                data-os-setup={
                  settingsTone && section.id === manifest.settingsSection ? "" : undefined
                }
                aria-label={
                  settingsTone && section.id === manifest.settingsSection
                    ? `${section.name}, ${manifest.name} is ${settingsStatePhrase}`
                    : undefined
                }
                aria-current={section.id === (current?.parent ?? current?.id) ? "page" : undefined}
                onClick={() => actions.navigateSection(win.id, section.id)}
              >
                {section.name}<AttentionMarker appId={manifest.id} sectionId={section.id} />
                {settingsTone && section.id === manifest.settingsSection ? (
                  <ProvenanceDot tone={settingsTone} />
                ) : null}
              </button>
            ))}
            {destinations.length > 2 ? <select className="os-window-more-nav" aria-label={`More ${manifest.name} sections`} value={destinations.slice(2).some(s => s.id === (current?.parent ?? current?.id)) ? current?.parent ?? current?.id : ""} onChange={event => { if (event.target.value) actions.navigateSection(win.id, event.target.value); }}><option value="">More…</option>{destinations.slice(2).map(s => <option key={s.id} value={s.id}>{s.name}</option>)}</select> : null}
          </nav>
        ) : null}
        <TrailRow fallback={current?.name ?? manifest.name} />
        <div ref={content} className="os-window-content" data-os-window-content>
          {/* THE REFUSED SURFACE (epic memql#4832, D6).
              A window can be open on an app this actor's rank does not clear,
              two ways that both happen: a desk restored from storage naming an
              app whose requirement the person no longer meets, and a role
              changed while they were signed in. openApp refuses the first
              OPEN, but neither path re-checks a window already on the desk.
              Rendering the app body anyway would run its reads and show the
              refusals one at a time, which says nothing about why. */}
          {/* THE SETUP SURFACE (design record 2026-09-06-configuration-readiness,
              5.3). Below the role refusal on purpose: "you may not open this"
              is a fact about the person and outranks "this is not wired up
              yet", which is a fact about the cluster.

              Only `unconfigured` gates. `unknown` -- the feed has not landed --
              renders the body, so a configured cluster never flashes a setup
              screen for a frame; `partial` renders it too, because mid-rollout
              is a state that resolves itself and a screen telling somebody to
              fix a deploy in progress is worse than a page that mostly works. */}
          {!accessAdmits(manifest.requires) ? (
            <SurfaceRefused
              surface={manifest.name}
              resource={manifest.requires}
              actorRole={actorRole}
            />
          ) : gate.state === "unconfigured" ? (
            <SurfaceUnconfigured
              surface={unmetIsSectionOnly ? (current?.name ?? manifest.name) : manifest.name}
              unmet={gate.unmet}
              descriptions={MODULE_DESCRIPTIONS}
              canSetUp={canConfigure(actorRole)}
              onSetUp={() => actions.navigateSection(win.id, manifest.settingsSection)}
            />
          ) : (
            /* THE BOUNDARY (epic memql#4895): a render error in this app
               stays in this window -- a Notice with the error's own sentence
               and a reload -- and is REPORTED with the app id and section
               exactly, which the boundary knows from here and the capture's
               focused-window guess does not. Keyed by window so one window's
               fault never carries into another's. */
            <WindowErrorBoundary key={win.id} app={manifest.id} section={current?.id ?? ""}>
              <WindowSearchContext.Provider value={searchHost}><AttentionDestination appId={manifest.id} sectionId={current?.id ?? ""} visible={!hidden}><Body
                sectionId={current?.id ?? ""}
                reportSetupState={reportSetupState}
                navigation={win.sectionNavigation}
                windowVisible={!hidden}
                navigate={(sectionId, options) => actions.navigateSection(win.id, sectionId, options?.fromContent ? "content" : "peer")}
                askContext={(tag) => openAsk(tag)}
                intent={win.intent}
                consumeIntent={(intentId) => actions.consumeWindowIntent(win.id, intentId)}
              /></AttentionDestination></WindowSearchContext.Provider>
            </WindowErrorBoundary>
          )}
        </div>
        </PageNavigationProvider>
      </div>
      {menu ? <ContextMenu x={menu.x} y={menu.y} label={`${manifest.name} window actions`} onClose={() => setMenu(null)} entries={[
        ...(ownDesk && ownDesk.windows.length > 1 ? [{ id: "swap", label: "Swap sides", onSelect: () => actions.swapSides(ownDesk.id) }] : []),
        ...state.shell.desks.filter(d => d.id !== ownDesk?.id).map((d) => ({ id: `move:${d.id}`, label: `Move to Desk ${state.shell.desks.indexOf(d) + 1}`, disabled: d.windows.length >= 2 || d.windows.some(id => !!state.shell.windows[id]?.fullscreenReturn), onSelect: () => { actions.throwToDesk(win.id, d.id); } })),
        { id: "new-desk", label: "Move to a new desktop", onSelect: () => { actions.throwToDesk(win.id, "new"); } },
        ...(logs ? [{ id: "logs", label: "Logs", onSelect: () => actions.navigateSection(win.id, logs.id) }] : []),
      ]} /> : null}
    </section>
  );
}
