import { useDraggable } from "@dnd-kit/core";
import { Maximize2, Minimize2, Minus, Settings2, X } from "lucide-react";

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
  requirementsFor,
  sectionsFor,
  type OsAppManifest,
} from "../system/registry";
import type { OsWindow } from "../system/windows";
import { useSession } from "./access";
import { Mark } from "./Mark";
import { useOs } from "./state";
import { WindowErrorBoundary } from "./WindowErrorBoundary";

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
}: {
  win: OsWindow;
  manifest: OsAppManifest;
  rect: Rect;
  focused: boolean;
  actorRole: string;
}) {
  const { actions } = useOs();
  const { openAsk } = useAsk();
  const { attributes, listeners, setNodeRef, transform, isDragging } = useDraggable({
    id: `window:${win.id}`,
  });

  const sections = sectionsFor(manifest);
  const current = sections.find((s) => s.id === win.sectionId) ?? sections[0];

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
  const settingsTone = markToneFor(appGate);
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
          aria-label={`Move ${manifest.name} -- drag to swap sides or throw to another desk`}
          {...listeners}
          {...attributes}
        >
          <Icon size={14} aria-hidden />
          <span className="os-window-title">{manifest.name}</span>
          {current && sections.length > 1 ? (
            <span className="os-window-crumb" aria-current="true">
              {current.name}
            </span>
          ) : null}
        </button>
        <div className="os-window-controls">
          <button
            type="button"
            className="os-icon-button"
            aria-label={`Ask about ${manifest.name}`}
            onClick={() => openAsk(contextTag)}
          >
            <Mark size={14} aria-hidden />
          </button>
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
              onClick={() => actions.navigateSection(win.id, manifest.settingsSection!)}
            >
              <Settings2 size={14} aria-hidden />
              {settingsTone ? <ProvenanceDot tone={settingsTone} /> : null}
            </button>
          ) : null}
          <button
            type="button"
            className="os-icon-button"
            aria-label={`Minimize ${manifest.name}`}
            onClick={() => actions.minimizeWindow(win.id)}
          >
            <Minus size={14} aria-hidden />
          </button>
          <button
            type="button"
            className="os-icon-button"
            aria-label={
              win.mode === "fullscreen" ? `Exit full screen` : `Full screen ${manifest.name}`
            }
            onClick={() => actions.toggleFullscreen(win.id)}
          >
            {win.mode === "fullscreen" ? <Minimize2 size={14} aria-hidden /> : <Maximize2 size={14} aria-hidden />}
          </button>
          <button
            type="button"
            className="os-icon-button os-window-close"
            aria-label={`Close ${manifest.name}`}
            onClick={() => actions.closeWindow(win.id)}
          >
            <X size={14} aria-hidden />
          </button>
        </div>
      </header>
      <div className="os-window-body">
        {sections.length > 1 ? (
          <nav className="os-window-nav" aria-label={`${manifest.name} sections`}>
            {sections.map((section) => (
              <button
                key={section.id}
                type="button"
                className="os-window-nav-item"
                data-os-setup={
                  settingsTone && section.id === manifest.settingsSection ? "" : undefined
                }
                aria-label={
                  settingsTone && section.id === manifest.settingsSection
                    ? `${section.name}, ${manifest.name} is ${settingsStatePhrase}`
                    : undefined
                }
                aria-current={section.id === current?.id ? "page" : undefined}
                onClick={() => actions.navigateSection(win.id, section.id)}
              >
                {section.name}
                {settingsTone && section.id === manifest.settingsSection ? (
                  <ProvenanceDot tone={settingsTone} />
                ) : null}
              </button>
            ))}
          </nav>
        ) : null}
        <div className="os-window-content" data-os-window-content>
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
              <Body
                sectionId={current?.id ?? ""}
                navigate={(sectionId) => actions.navigateSection(win.id, sectionId)}
                askContext={(tag) => openAsk(tag)}
                intent={win.intent}
                consumeIntent={(intentId) => actions.consumeWindowIntent(win.id, intentId)}
              />
            </WindowErrorBoundary>
          )}
        </div>
      </div>
    </section>
  );
}
