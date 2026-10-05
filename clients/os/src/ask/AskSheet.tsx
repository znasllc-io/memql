import { useEffect, useState, type CSSProperties } from "react";

import { useOs } from "../chrome/state";
import { useAsk } from "./AskProvider";
import { AskSurface, FLEET_ROUTING_SECTION } from "./AskSurface";

// The Ask sheet: anchored above the dock (a bottom sheet on phones via
// CSS), or expanded over the same work area as maximized apps. It stays
// outside the desktop's window count (spec D6); resizing never remounts Ask.
// The desk stays interactive while MemQL works; Escape closes the overlay.
//
// Both entry points retain the composer while the shared readiness check runs.

export function AskSheet({ dockReserve = 0 }: { dockReserve?: number }) {
  const { sheet, closeAsk, transport, voice, settings, availability, conversation, liveVoice } = useAsk();
  const { actions } = useOs();
  const [maximized, setMaximized] = useState(false);

  useEffect(() => {
    if (!sheet.open) return;
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") closeAsk();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [sheet.open, closeAsk]);

  if (!sheet.open) return null;
  return (
    <div
      className="os-ask-backdrop"
      onPointerDown={(event) => {
        if (event.target === event.currentTarget) closeAsk();
      }}
    >
      <div
        className="os-ask-sheet"
        data-maximized={maximized || undefined}
        style={{ "--ask-dock-reserve": `${dockReserve}px` } as CSSProperties}
        data-os-sheet
        role="dialog"
        aria-modal="false"
        aria-label="Ask"
      >
        <AskSurface
          transport={transport}
          availability={availability}
          conversation={conversation}
          liveVoice={liveVoice}
          onClose={closeAsk}
          maximized={maximized}
          onToggleMaximize={() => setMaximized(value => !value)}
          onOpenFleet={() => { actions.openApp("fleet"); closeAsk(); }}
          onManageRoutes={() => { actions.openApp("fleet", FLEET_ROUTING_SECTION); closeAsk(); }}
          voicePorts={voice}
          settings={settings}
          context={sheet.context}
          contextLabel={sheet.contextLabel}
          variant="sheet"
          onOpenFile={(fileId) => { actions.openApp("files", "browse", { fileId }); }}
          autoFocus
        />
      </div>
    </div>
  );
}
