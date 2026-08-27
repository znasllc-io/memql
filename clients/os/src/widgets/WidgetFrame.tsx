import { useState } from "react";
import { MoreHorizontal } from "lucide-react";

import type { OsWidgetManifest } from "../system/registry";

// A widget's card chrome (spec D5): token-carrying root (data-os-widget),
// quiet header, one overflow action -- Remove. The body is the manifest's
// component; widgets do something without opening an app.

export function WidgetFrame({
  manifest,
  onRemove,
}: {
  manifest: OsWidgetManifest;
  onRemove: () => void;
}) {
  const [menuOpen, setMenuOpen] = useState(false);
  const Icon = manifest.icon;
  const Body = manifest.component;

  return (
    <section className="os-widget" data-os-widget={manifest.id} aria-label={manifest.name}>
      <header className="os-widget-head">
        <Icon size={13} aria-hidden />
        <h2 className="os-widget-name">{manifest.name}</h2>
        <div className="os-widget-menu-anchor">
          <button
            type="button"
            className="os-icon-button"
            aria-label={`${manifest.name} widget menu`}
            aria-haspopup="menu"
            aria-expanded={menuOpen}
            onClick={() => setMenuOpen((v) => !v)}
          >
            <MoreHorizontal size={14} aria-hidden />
          </button>
          {menuOpen ? (
            <div role="menu" className="os-menu">
              <button
                type="button"
                role="menuitem"
                className="os-menu-item"
                onClick={() => {
                  setMenuOpen(false);
                  onRemove();
                }}
              >
                Remove from desk
              </button>
            </div>
          ) : null}
        </div>
      </header>
      <div className="os-widget-body">
        <Body />
      </div>
    </section>
  );
}
