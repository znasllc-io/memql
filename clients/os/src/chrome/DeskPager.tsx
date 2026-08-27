import { useDroppable } from "@dnd-kit/core";
import { Plus } from "lucide-react";

import { DESK_CAP, type Desk } from "../system/desks";
import { useOs } from "./state";

// The desk pager (spec A): dots above the dock. Click switches; a window
// dragged onto a dot is thrown there; a full dot dims during a window
// drag; "+" adds a user desk. The active desk is announced politely by
// the Desktop (aria-live), not here.

function PagerDot({ desk, index, active, draggingWindow }: {
  desk: Desk;
  index: number;
  active: boolean;
  draggingWindow: boolean;
}) {
  const { actions } = useOs();
  const full = desk.windows.length >= DESK_CAP;
  const { setNodeRef, isOver } = useDroppable({ id: `pagerdot:${desk.id}`, disabled: full });
  return (
    <button
      ref={setNodeRef}
      type="button"
      className="os-pager-dot"
      data-active={active || undefined}
      data-over={(isOver && !full) || undefined}
      data-refuses={(draggingWindow && full && !active) || undefined}
      aria-label={`Desk ${index + 1}${active ? ", current" : ""}${full ? ", full" : ""}`}
      aria-current={active || undefined}
      onClick={() => actions.switchDesk(desk.id)}
    />
  );
}

function NewDeskDrop({ visible }: { visible: boolean }) {
  const { setNodeRef, isOver } = useDroppable({ id: "pagerdot:new" });
  if (!visible) return null;
  return (
    <span ref={setNodeRef} className="os-pager-new" data-over={isOver || undefined} aria-hidden="true">
      <Plus size={10} aria-hidden />
    </span>
  );
}

export function DeskPager({ draggingWindow }: { draggingWindow: boolean }) {
  const { state, actions } = useOs();
  const { desks, activeDeskId } = state.shell;
  return (
    <nav className="os-pager" aria-label="Desks">
      {desks.map((desk, index) => (
        <PagerDot
          key={desk.id}
          desk={desk}
          index={index}
          active={desk.id === activeDeskId}
          draggingWindow={draggingWindow}
        />
      ))}
      {draggingWindow ? (
        <NewDeskDrop visible />
      ) : (
        <button
          type="button"
          className="os-pager-add"
          aria-label="New desk"
          onClick={() => actions.addDesk()}
        >
          <Plus size={10} aria-hidden />
        </button>
      )}
    </nav>
  );
}
