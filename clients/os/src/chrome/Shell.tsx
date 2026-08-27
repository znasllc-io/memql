import { useCallback, useState, type ReactNode } from "react";

import type { ChromeLayout } from "../app/layout";
import type { ProfileAccess } from "../modules/profile/access";
import { Profile } from "../modules/profile/Profile";
import { Research } from "../research/Research";
import { emptySlots, occupy, slotCap, vacate, type SlotId, type SlotState } from "../slots/manager";
import { Slot } from "../slots/Slot";
import { Launcher } from "./Launcher";
import { ModeSwitcher } from "./ModeSwitcher";
import { SignOut } from "./SignOut";

export function Shell({
  layout,
  onSignOut,
  access = null,
}: {
  layout: ChromeLayout;
  onSignOut: () => void;
  access?: ProfileAccess | null;
}) {
  const [slots, setSlots] = useState<SlotState>(emptySlots);
  const cap = slotCap(layout);
  const showSlots = cap > 0;

  const launchProfile = useCallback(() => {
    setSlots((current) => occupy(current, layout, "profile").state);
  }, [layout]);

  const closeSlot = useCallback((id: SlotId) => {
    setSlots((current) => vacate(current, id));
  }, []);

  return (
    <div className="os-root" data-os-root data-layout={layout}>
      <Launcher layout={layout} onLaunchProfile={showSlots ? launchProfile : undefined} />
      <div className="os-chrome-actions">
        <ModeSwitcher />
        <SignOut onSignOut={onSignOut} />
      </div>
      {showSlots ? (
        <div className="os-slots" data-os-slots>
          <Slot id="a" occupant={slots.a} onVacate={closeSlot}>
            {mountModule(slots.a, access)}
          </Slot>
          {cap >= 2 ? (
            <Slot id="b" occupant={slots.b} onVacate={closeSlot}>
              {mountModule(slots.b, access)}
            </Slot>
          ) : null}
        </div>
      ) : (
        <main className="os-phone-allowlist" data-os-phone-allowlist>
          <Profile access={access} />
        </main>
      )}
      <Research host={layout === "phone" ? "sheet" : "strip"} />
    </div>
  );
}

function mountModule(moduleId: string | null, access: ProfileAccess | null): ReactNode {
  if (moduleId === "profile") return <Profile access={access} />;
  return null;
}
