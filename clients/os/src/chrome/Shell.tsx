import type { ChromeLayout } from "../app/layout";
import { Launcher } from "./Launcher";
import { ModeSwitcher } from "./ModeSwitcher";
import { SignOut } from "./SignOut";

export function Shell({
  layout,
  onSignOut,
}: {
  layout: ChromeLayout;
  onSignOut: () => void;
}) {
  const desktop = layout === "desktop";
  return (
    <div className="os-root" data-os-root data-layout={layout}>
      <Launcher layout={layout} />
      <div className="os-chrome-actions">
        <ModeSwitcher />
        <SignOut onSignOut={onSignOut} />
      </div>
      {desktop ? (
        <div className="os-slots" data-slots>
          <section className="os-slot" data-slot="a" aria-label="Reserved slot" />
          <section className="os-slot" data-slot="b" aria-label="Reserved slot" />
        </div>
      ) : null}
      <main className="os-desktop" data-desktop>
        <p className="os-empty">Desktop</p>
      </main>
    </div>
  );
}
