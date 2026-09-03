import { useDraggable } from "@dnd-kit/core";
import { Maximize2, Minimize2, Minus, Settings2, Sparkles, X } from "lucide-react";

import { useAsk } from "../ask/AskProvider";
import { SurfaceRefused } from "../kit/RankStates";
import type { Rect } from "../system/placement";
import { sectionsForRole, type OsAppManifest } from "../system/registry";
import { requirementFloor, roleAdmits } from "../system/roles";
import type { OsWindow } from "../system/windows";
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

  const sections = sectionsForRole(manifest, actorRole);
  const current = sections.find((s) => s.id === win.sectionId) ?? sections[0];
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
            <Sparkles size={14} aria-hidden />
          </button>
          {manifest.settingsSection ? (
            <button
              type="button"
              className="os-icon-button"
              aria-label={`${manifest.name} settings`}
              onClick={() => actions.navigateSection(win.id, manifest.settingsSection!)}
            >
              <Settings2 size={14} aria-hidden />
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
                aria-current={section.id === current?.id ? "page" : undefined}
                onClick={() => actions.navigateSection(win.id, section.id)}
              >
                {section.name}
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
          {roleAdmits(actorRole, manifest.roles) ? (
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
          ) : (
            <SurfaceRefused
              surface={manifest.name}
              required={requirementFloor(manifest.roles)}
              actorRole={actorRole}
            />
          )}
        </div>
      </div>
    </section>
  );
}
