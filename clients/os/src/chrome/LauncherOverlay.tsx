import { useEffect, useMemo, useRef, useState } from "react";
import { Palette } from "lucide-react";

import { Mark } from "./Mark";
import { appsForRole, widgetsForRole } from "../system/registry";
import { useOs } from "./state";

// The Launcher (spec A): a full-screen glass overlay -- search, the
// role-filtered app grid, a Widgets tab, and the theme-marketplace tile.
// One of Squada One's three sanctioned appearances is the wordmark row here.
//
// The Themes tile is the one tile that does not launch an app. It hands the
// shell a callback rather than calling `actions.openApp`, because the
// marketplace is a drawer over the live desktop -- see themes/ThemeStore.tsx
// for why that is the shape it has to be.

export function LauncherOverlay({
  open,
  onClose,
  onOpenThemes,
}: {
  open: boolean;
  onClose: () => void;
  onOpenThemes: () => void;
}) {
  const { registry, actorRole, ladderLoaded, actions } = useOs();
  const [tab, setTab] = useState<"apps" | "widgets">("apps");
  const [query, setQuery] = useState("");
  const searchRef = useRef<HTMLInputElement | null>(null);
  const gridRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (open) {
      setQuery("");
      setTab("apps");
      searchRef.current?.focus();
    }
  }, [open]);

  // `ladderLoaded` is a dep even though it is not read in the body: appsForRole
  // calls roleAdmits, which reads the role LADDER out of band (module state),
  // so the ladder landing after the role is a change this memo must see or it
  // keeps the empty-ladder answer -- every gated app hidden -- for the session
  // (memql#4857). The same reason it rides the widgets filter below.
  const apps = useMemo(() => {
    const admitted = appsForRole(registry, actorRole);
    const q = query.trim().toLowerCase();
    return q ? admitted.filter((a) => a.name.toLowerCase().includes(q)) : admitted;
  }, [registry, actorRole, ladderLoaded, query]);

  const widgets = useMemo(
    () => widgetsForRole(registry, actorRole),
    [registry, actorRole, ladderLoaded],
  );

  if (!open) return null;

  function launch(appId: string) {
    onClose();
    actions.openApp(appId);
  }

  function onGridKeyDown(event: React.KeyboardEvent) {
    const buttons = Array.from(
      gridRef.current?.querySelectorAll<HTMLButtonElement>("button[data-tile]") ?? [],
    );
    const at = buttons.indexOf(document.activeElement as HTMLButtonElement);
    if (at < 0) return;
    const cols = 4;
    const move: Record<string, number> = {
      ArrowRight: at + 1,
      ArrowLeft: at - 1,
      ArrowDown: at + cols,
      ArrowUp: at - cols,
    };
    const to = move[event.key];
    if (to !== undefined) {
      event.preventDefault();
      buttons[Math.max(0, Math.min(buttons.length - 1, to))]?.focus();
    }
  }

  return (
    <div
      className="os-launcher"
      role="dialog"
      aria-modal="true"
      aria-label="Launcher"
      onPointerDown={(event) => {
        if (event.target === event.currentTarget) onClose();
      }}
      onKeyDown={(event) => {
        if (event.key === "Escape") onClose();
      }}
    >
      <div className="os-launcher-panel">
        <div className="os-launcher-brand">
          <Mark className="os-launcher-mark" />
          <span className="os-launcher-wordmark">MemQL OS</span>
        </div>
        <input
          ref={searchRef}
          className="os-launcher-search"
          placeholder="Search apps"
          aria-label="Search apps"
          value={query}
          onChange={(event) => setQuery(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Enter" && apps[0]) launch(apps[0].id);
          }}
        />
        <div className="os-launcher-tabs" role="tablist" aria-label="Launcher sections">
          <button
            type="button"
            role="tab"
            aria-selected={tab === "apps"}
            className="os-launcher-tab"
            onClick={() => setTab("apps")}
          >
            Apps
          </button>
          <button
            type="button"
            role="tab"
            aria-selected={tab === "widgets"}
            className="os-launcher-tab"
            onClick={() => setTab("widgets")}
          >
            Widgets
          </button>
        </div>
        {tab === "apps" ? (
          <div className="os-launcher-grid" ref={gridRef} onKeyDown={onGridKeyDown}>
            {apps.map((app) => {
              const Icon = app.icon;
              return (
                <button
                  key={app.id}
                  type="button"
                  data-tile
                  className="os-tile"
                  onClick={() => launch(app.id)}
                >
                  <Icon size={26} aria-hidden />
                  <span>{app.name}</span>
                </button>
              );
            })}
            {/* Live since epic memql#4745. It is not an app and does not
                launch one: the marketplace previews against THIS desktop, so
                it opens a drawer and closes the Launcher on the way. */}
            <button type="button" data-tile className="os-tile" onClick={onOpenThemes}>
              <Palette size={26} aria-hidden />
              <span>Themes</span>
              <span className="os-caption">Try one on</span>
            </button>
            {apps.length === 0 ? (
              <p className="os-caption os-launcher-empty">No app matches "{query}".</p>
            ) : null}
          </div>
        ) : (
          <div className="os-launcher-grid" ref={gridRef} onKeyDown={onGridKeyDown}>
            {widgets.map((widget) => {
              const Icon = widget.icon;
              return (
                <button
                  key={widget.id}
                  type="button"
                  data-tile
                  className="os-tile"
                  onClick={() => {
                    const ok = actions.addWidget(widget.id);
                    if (ok) onClose();
                  }}
                >
                  <Icon size={26} aria-hidden />
                  <span>{widget.name}</span>
                  <span className="os-caption">Add to desk</span>
                </button>
              );
            })}
          </div>
        )}
      </div>
    </div>
  );
}
