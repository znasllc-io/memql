import { useEffect } from "react";

import { useAsk } from "./AskProvider";
import { AskSurface } from "./AskSurface";
import { useMakeGoal } from "./useMakeGoal";

// The Ask sheet: anchored above the dock (a bottom sheet on phones via
// CSS). Never a window, never counts against the desk cap (spec D6).
// Esc closes from anywhere -- the sheet is modal over the desk, so the
// close key cannot depend on where focus happens to sit.

export function AskSheet() {
  const { sheet, closeAsk, transport, voice, settings } = useAsk();
  const makeGoal = useMakeGoal();

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
        data-os-sheet
        role="dialog"
        aria-modal="true"
        aria-label="Ask"
        onKeyDown={(event) => {
          if (event.key === "Escape") closeAsk();
        }}
      >
        <AskSurface
          transport={transport}
          voicePorts={voice}
          settings={settings}
          context={sheet.context}
          variant="sheet"
          autoFocus
          makeGoal={makeGoal}
        />
      </div>
    </div>
  );
}
