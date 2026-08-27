import { useAsk } from "./AskProvider";
import { AskSurface } from "./AskSurface";

// The Ask sheet: anchored above the dock (a bottom sheet on phones via
// CSS). Never a window, never counts against the desk cap (spec D6).

export function AskSheet() {
  const { sheet, closeAsk, transport } = useAsk();
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
        <AskSurface transport={transport} context={sheet.context} variant="sheet" autoFocus />
      </div>
    </div>
  );
}
