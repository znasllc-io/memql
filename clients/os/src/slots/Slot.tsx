import type { ReactNode } from "react";

import type { SlotId } from "./manager";

export function Slot({
  id,
  occupant,
  onVacate,
  children,
}: {
  id: SlotId;
  occupant: string | null;
  onVacate: (id: SlotId) => void;
  children: ReactNode;
}) {
  return (
    <section
      className="os-slot"
      data-os-slot={id}
      data-os-tokens
      data-occupant={occupant ?? undefined}
      aria-label={occupant ? `Slot ${id}: ${occupant}` : `Slot ${id}`}
    >
      {occupant ? (
        <button
          type="button"
          className="os-slot-close"
          aria-label={`Close ${occupant}`}
          onClick={() => onVacate(id)}
        >
          Close
        </button>
      ) : null}
      {children}
    </section>
  );
}
