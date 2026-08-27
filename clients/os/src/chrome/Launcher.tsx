import type { ChromeLayout } from "../app/layout";
import { Mark } from "./Mark";

export function Launcher({
  layout,
  onLaunchProfile,
}: {
  layout: ChromeLayout;
  onLaunchProfile?: () => void;
}) {
  const showTiles = layout !== "phone";
  return (
    <header className="os-launcher" data-launcher data-layout={layout}>
      <span className="os-brand">
        <Mark className="os-mark" />
        <span className="os-wordmark">MemQL</span>
      </span>
      {showTiles ? (
        <nav className="os-launcher-tiles" aria-label="Modules">
          <button
            type="button"
            className="os-tile"
            data-os-launch="profile"
            onClick={onLaunchProfile}
          >
            Profile
          </button>
          <button
            type="button"
            className="os-tile os-tile-soon"
            data-coming-soon
            data-os-coming-soon
            data-os-launch="coming-soon"
          >
            Coming soon
          </button>
        </nav>
      ) : null}
    </header>
  );
}
