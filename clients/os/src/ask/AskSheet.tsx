import { useEffect, useState, type CSSProperties } from "react";

import { useOs } from "../chrome/state";
import { useAsk } from "./AskProvider";
import { AskSurface } from "./AskSurface";

// The Ask sheet: anchored above the dock, or filling the phone's content
// area below its header. It stays
// outside the desktop's window count (spec D6); resizing never remounts Ask.
// The desk stays interactive while MemQL works; Escape closes the overlay.
//
// Both entry points retain the composer while the shared readiness check runs.

export function AskSheet({ dockReserve = 0, embedded = false, onOpenApp }: { dockReserve?: number; embedded?: boolean; onOpenApp?: (appId: string, sectionId?: string, payload?: Record<string, unknown>) => void }) {
  const { sheet, closeAsk, transport, voice, settings, availability, conversation, liveVoice } = useAsk();
  const { actions } = useOs();
  const [maximized, setMaximized] = useState(false);
  const openApp = onOpenApp ?? actions.openApp;

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
          onToggleMaximize={embedded ? undefined : () => setMaximized(value => !value)}
          onOpenFleet={() => { openApp("fleet"); closeAsk(); }}
          voicePorts={voice}
          settings={settings}
          context={sheet.context}
          contextLabel={sheet.contextLabel}
          variant="sheet"
          onOpenFile={(fileId) => { openApp("files", "browse", { fileId }); if (embedded) closeAsk(); }}
          onOpenWork={(runId, conversationId) => { openApp("nexus", "runs", { runId, conversationId }); closeAsk(); }}
          autoFocus
        />
      </div>
    </div>
  );
}
